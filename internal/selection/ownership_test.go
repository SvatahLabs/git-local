package selection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
)

func TestOwnershipParserAndAliasCollisions(t *testing.T) {
	oid := strings.Repeat("a", 40)
	good := []byte("100644 " + oid + " 2\tname\twith-tab\x00")
	entries, err := parseEntries(good, true)
	if err != nil || len(entries) != 1 || entries[0].path != "name\twith-tab" {
		t.Fatal("tab filename parsed incorrectly", err)
	}
	for _, bad := range []string{"100644 " + oid + " 0\ta", "broken\x00", "100644 " + oid + " 4\ta\x00", "100644 blob bad-id\ta\x00", "040000 " + oid + " 0\t../escape/\x00"} {
		if _, err := parseEntries([]byte(bad), true); err == nil {
			t.Fatalf("accepted malformed record %q", bad)
		}
	}
	cases := []struct {
		owned    string
		s        State
		conflict bool
	}{
		{"NOTES/private", State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}}, true},
		{"café/file", State{SchemaVersion: 2, Include: []Rule{{"cafe\u0301", "directory"}}}, true},
		{"notes", State{SchemaVersion: 2, Include: []Rule{{"notes/child", "file"}}}, true},
		{"notes/private", State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes/private"}}, false},
		{"notes/shared", State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes/private"}}, true},
	}
	for _, tc := range cases {
		err := (Ownership{Paths: []string{tc.owned}}).Check(tc.s)
		if errors.Is(err, storage.ErrConflict) != tc.conflict {
			t.Fatalf("%q: %v", tc.owned, err)
		}
	}
}

type unavailableGit struct{}

func (unavailableGit) Main(context.Context, string, string, ...string) ([]byte, error) {
	return nil, errors.New("ownership unavailable")
}
func (g unavailableGit) MainListing(ctx context.Context, root, admin string, args ...string) ([]byte, error) {
	return g.Main(ctx, root, admin, args...)
}
func TestUnavailableOwnershipIsNotEmpty(t *testing.T) {
	if _, err := InspectOwnership(context.Background(), unavailableGit{}, "", " "); err == nil {
		t.Fatal("unavailable ownership accepted")
	}
}

// CheckPaths decides, and names the owned path, exactly as Check of a
// directory rule for each path does.
func TestCheckPathsMatchesCheck(t *testing.T) {
	owned := []string{"NOTES/private", "café/file", "docs", "notes2", "a/b/c", "x/ＡＢ/y", "zz/deep/one", "zz/deep/two"}
	for _, p := range []string{"notes", "notes/private", "notes/private/x", "café", "docs/readme", "notes2", "notes3", "a", "a/b", "a/b/c/d", "a/bc", "x/ab", "zz", "zz/deep", "zz/other", "fresh/file", "../escape", ""} {
		s := Empty()
		s.Include = []Rule{{Path: p, Kind: "directory"}}
		want := (Ownership{Paths: owned}).Check(s)
		got := (Ownership{Paths: owned}).CheckPaths([]string{"fresh", p})
		if (want == nil) != (got == nil) || want != nil && want.Error() != got.Error() {
			t.Fatalf("%q: CheckPaths %v, Check %v", p, got, want)
		}
	}
}

// Checking many paths folds each owned path once rather than once per
// checked path. Not parallel: it replaces FoldOwned.
func TestCheckPathsFoldsEachOwnedPathOnce(t *testing.T) {
	folds := 0
	FoldOwned = func(p string) string { folds++; return storage.AliasKey(p) }
	defer func() { FoldOwned = storage.AliasKey }()
	owned := make([]string, 20000)
	for i := range owned {
		owned[i] = fmt.Sprintf("main/file-%05d", i)
	}
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("notes/file-%03d", i)
	}
	if err := (Ownership{Paths: owned}).CheckPaths(paths); err != nil {
		t.Fatal(err)
	}
	if folds > len(owned)+len(paths) {
		t.Fatalf("checking %d paths against %d owned paths made %d folds", len(paths), len(owned), folds)
	}
	if err := (Ownership{Paths: owned}).CheckPaths(append(paths, "main")); !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), `"main/file-00000"`) {
		t.Fatalf("owned descendant not reported: %v", err)
	}
}

// GL-125: after main changed since evidence a journal recorded, the message
// says to retry after Git operations finish only while a Git command holds
// the index or HEAD; otherwise waiting changes nothing.
func TestRevalidateSuggestsWaitingOnlyWhileGitIsBusy(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	admin := filepath.Join(repo.Root, ".git")
	owner, err := InspectOwnership(ctx, r, repo.Root, admin)
	if err != nil {
		t.Fatal(err)
	}
	repo.Git("commit", "-q", "--allow-empty", "-m", "moved")
	err = owner.RevalidateRecorded(ctx, r, repo.Root, admin)
	if err == nil || !strings.Contains(err.Error(), "main HEAD or index changed") || strings.Contains(err.Error(), "retry") {
		t.Fatalf("revalidate after a commit: %v", err)
	}
	for _, lock := range []string{"index.lock", "HEAD.lock"} {
		if err := os.WriteFile(filepath.Join(admin, lock), nil, 0600); err != nil {
			t.Fatal(err)
		}
		err = owner.RevalidateRecorded(ctx, r, repo.Root, admin)
		if err == nil || !strings.Contains(err.Error(), "retry after Git operations finish") {
			t.Fatalf("revalidate while %s exists: %v", lock, err)
		}
		if err := os.Remove(filepath.Join(admin, lock)); err != nil {
			t.Fatal(err)
		}
	}
}
