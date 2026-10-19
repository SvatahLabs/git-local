package gitexec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// capFixture stages 400 files, so the index listing, the staged-name diff and
// a private tree holding the same names are each larger than 8 KiB.
func capFixture(t *testing.T) (*testutil.Repo, *Runner, string, string) {
	t.Helper()
	repo := testutil.New(t)
	for i := 0; i < 400; i++ {
		path := filepath.Join(repo.Root, "src", fmt.Sprintf("file-with-a-long-name-%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("tracked\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	repo.Git("add", "src")
	r := newRunner(t, repo)
	ctx := context.Background()
	store := filepath.Join(t.TempDir(), "history.git")
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	blob, err := r.History(ctx, store, strings.NewReader("tracked\n"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	var entries bytes.Buffer
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&entries, "100644 blob %s\tfile-with-a-long-name-%04d.txt\x00", strings.TrimSpace(string(blob)), i)
	}
	tree, err := r.History(ctx, store, &entries, "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	return repo, r, store, strings.TrimSpace(string(tree))
}

func lowerOutputCap(t *testing.T) {
	saved := MaxOutput
	MaxOutput = 8 << 10
	t.Cleanup(func() { MaxOutput = saved })
}

// GL-105: os/exec copies output with the buffer's ReadFrom, which bypassed the
// capped Write, so no buffered call ever reported the safety limit.
func TestBufferedOutputCapIsEnforced(t *testing.T) {
	repo, r, store, tree := capFixture(t)
	ctx := context.Background()
	admin := filepath.Join(repo.Root, ".git")
	lowerOutputCap(t)
	if out, err := r.Main(ctx, repo.Root, admin, "ls-files", "--stage", "-z"); err == nil || err.Error() != "Git output exceeded safety limit" {
		t.Fatalf("capped main listing returned %d bytes, err %v", len(out), err)
	}
	if out, err := r.History(ctx, store, nil, "ls-tree", "-z", tree); err == nil || err.Error() != "Git output exceeded safety limit" {
		t.Fatalf("capped history listing returned %d bytes, err %v", len(out), err)
	}
	if out, err := r.Guard(ctx, repo.Root, "", "diff", "--cached", "--name-only", "-z"); err == nil || err.Error() != "Git output exceeded safety limit" {
		t.Fatalf("capped guard query returned %d bytes, err %v", len(out), err)
	}
	// Output below the cap is still returned whole.
	if out, err := r.Main(ctx, repo.Root, admin, "rev-parse", "--absolute-git-dir"); err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("small output: %q, %v", out, err)
	}
}

// Listings that grow with the index or history are explicitly uncapped and
// return every record; the capped variants of the same queries refuse.
func TestListingsAreNotCapped(t *testing.T) {
	repo, r, store, tree := capFixture(t)
	ctx := context.Background()
	admin := filepath.Join(repo.Root, ".git")
	lowerOutputCap(t)
	listing, err := r.MainListing(ctx, repo.Root, admin, "ls-files", "--stage", "-z")
	if err != nil || bytes.Count(listing, []byte{0}) != 400 {
		t.Fatalf("main listing: %d records, %v", bytes.Count(listing, []byte{0}), err)
	}
	entries, err := r.HistoryListing(ctx, store, nil, "ls-tree", "-z", tree)
	if err != nil || bytes.Count(entries, []byte{0}) != 400 {
		t.Fatalf("history listing: %d records, %v", bytes.Count(entries, []byte{0}), err)
	}
	staged, err := r.GuardListing(ctx, repo.Root, "", nil, "diff", "--cached", "--name-only", "-z")
	if err != nil || bytes.Count(staged, []byte{0}) != 400 {
		t.Fatalf("guard listing: %d records, %v", bytes.Count(staged, []byte{0}), err)
	}
	var paths []string
	for i := 0; i < 400; i++ {
		paths = append(paths, fmt.Sprintf("src/file-with-a-long-name-%04d.txt", i))
	}
	ignored, err := r.CheckIgnore(ctx, repo.Root, admin, paths)
	if err != nil || len(ignored) != 400 {
		t.Fatalf("check-ignore: %d results, %v", len(ignored), err)
	}
	// Content reads and patches stay capped, and other commands can't use the
	// uncapped calls.
	for _, args := range [][]string{{"cat-file", "blob", tree}, {"diff-tree", "-p", tree, tree}, {"diff-tree", "-r", "-u", tree, tree}, {"diff-tree", "-U1", tree, tree}, {"diff-tree", "--unified=2", tree, tree}, {"diff-tree", "--cc", tree, tree}, {"diff-tree", "--patch-with-stat", tree, tree}, {"diff-tree", "--binary", tree, tree}, {"fsck"}} {
		if _, err := r.HistoryListing(ctx, store, nil, args...); err == nil {
			t.Fatalf("HistoryListing accepted %v", args)
		}
	}
	for _, args := range [][]string{{"cat-file", "blob", ":0:src/file-with-a-long-name-0000.txt"}, {"diff", "--cached"}} {
		if _, err := r.GuardListing(ctx, repo.Root, "", nil, args...); err == nil {
			t.Fatalf("GuardListing accepted %v", args)
		}
	}
}
