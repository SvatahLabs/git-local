package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
)

// restoreFolds restores n changed files in a repository whose main index
// holds owned paths, and returns the alias folds its ownership checks made.
func restoreFolds(t *testing.T, n, owned int) int {
	t.Helper()
	repo, r, _ := fixture(t)
	root := repo.Root
	blob := strings.TrimSpace(string(repo.Git("hash-object", "-w", "app.txt")))
	var entries strings.Builder
	for i := 0; i < owned; i++ {
		fmt.Fprintf(&entries, "100644 %s\tmain/file-%05d\n", blob, i)
	}
	repo.Input(root, entries.String(), "update-index", "--add", "--index-info")
	for i := 0; i < n; i++ {
		write(t, root, fmt.Sprintf("notes/file-%03d", i), "source")
	}
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	for i := 0; i < n; i++ {
		write(t, root, fmt.Sprintf("notes/file-%03d", i), "changed")
	}
	save(t, r, root)
	folds := 0
	selection.FoldOwned = func(p string) string { folds++; return storage.AliasKey(p) }
	defer func() { selection.FoldOwned = storage.AliasKey }()
	result, err := Execute(ctx, r, root, "", source, []string{"notes"}, true)
	if err != nil || !result.Changed {
		t.Fatalf("restore %d files: %+v %v", n, result, err)
	}
	for i := 0; i < n; i++ {
		if data, err := os.ReadFile(filepath.Join(root, "notes", fmt.Sprintf("file-%03d", i))); err != nil || string(data) != "source" {
			t.Fatalf("restored file %d: %q %v", i, data, err)
		}
	}
	return folds
}

// Planning and the guard check every restored path and stage against main
// ownership with one fold of each owned path, so a restore's ownership checks
// cost the owned paths plus the restored ones. Before, each path was checked
// against every owned path: about four folds per restored file and owned
// path. Not parallel: it replaces selection.FoldOwned.
func TestRestoreOwnershipChecksDontGrowWithMainIndex(t *testing.T) {
	const owned, n = 3000, 4
	folds := restoreFolds(t, n, owned)
	t.Logf("alias folds restoring %d files with %d owned paths: %d", n, owned, folds)
	if folds > 3*owned {
		t.Fatalf("restoring %d files made %d ownership folds against %d owned paths", n, folds, owned)
	}
}

// restoreChecks restores n changed files and returns the path checks the
// restore's guard made. how is "quiet"; "rewrite", which replaces the main
// index with an identical copy before each operation (as a tool polling git
// status does); "symlinked-head", a HEAD written as a symlink (which
// core.preferSymlinkRefs does), a state MainState never trusts; or
// "reftable", a repository with the reftable ref backend.
func restoreChecks(t *testing.T, n int, how string) int {
	t.Helper()
	newRepo := testutil.New
	if how == "reftable" {
		newRepo = testutil.NewReftable
	}
	repo, r, w := fixtureFrom(t, newRepo(t))
	root := repo.Root
	if how == "symlinked-head" {
		head := filepath.Join(w.Admin, "HEAD")
		if err := os.Remove(head); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("refs/heads/main", head); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(repo.Git("symbolic-ref", "HEAD"))); got != "refs/heads/main" {
			t.Fatalf("Git reads the symlinked HEAD as %q", got)
		}
	}
	rewrite := how == "rewrite"
	for i := 0; i < n; i++ {
		write(t, root, fmt.Sprintf("notes/file-%03d", i), "source")
	}
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	for i := 0; i < n; i++ {
		write(t, root, fmt.Sprintf("notes/file-%03d", i), "changed")
	}
	save(t, r, root)
	checks, rewrites := 0, 0
	pathChecked = func(string) { checks++ }
	defer func() { pathChecked = func(string) {} }()
	_, err := execute(ctx, r, root, "", source, []string{"notes"}, true, func(at string) error {
		if !rewrite || !strings.HasPrefix(at, "before-progress:") {
			return nil
		}
		rewrites++
		index := filepath.Join(w.Admin, "index")
		b, e := os.ReadFile(index)
		if e != nil {
			return e
		}
		if e = os.WriteFile(index+".new", b, 0644); e != nil {
			return e
		}
		return os.Rename(index+".new", index)
	})
	if err != nil {
		t.Fatalf("restore (%s): %v", how, err)
	}
	if rewrite && rewrites < n {
		t.Fatalf("index rewritten %d times for %d files", rewrites, n)
	}
	for i := 0; i < n; i++ {
		if data, err := os.ReadFile(filepath.Join(root, "notes", fmt.Sprintf("file-%03d", i))); err != nil || string(data) != "source" {
			t.Fatalf("restored file %d: %q %v", i, data, err)
		}
	}
	return checks
}

// An index rewritten without entry changes between operations makes the
// guard read ownership and the branch again, not every operation's image.
// Before, each rewrite forced a full check, so a tool rewriting the index
// during a long restore made it quadratic again. So does a main state that
// can't show the next change (it isn't settled): the next step reads
// ownership again; it used to force a full check at every step. A reftable
// repository restores too (its refs/heads is a file, which stopped every
// restore). Not parallel: it replaces pathChecked.
func TestRestoreIndexRewritesRecheckOnlyOwnership(t *testing.T) {
	const n = 6
	quiet := restoreChecks(t, n, "quiet")
	for _, how := range []string{"rewrite", "symlinked-head", "reftable"} {
		if how == "symlinked-head" && runtime.GOOS == "windows" {
			continue
		}
		checks := restoreChecks(t, n, how)
		t.Logf("path checks for %d files: %d, %s %d", n, quiet, how, checks)
		if quiet == 0 || checks > quiet+n {
			t.Errorf("%s raised the path checks from %d to %d", how, quiet, checks)
		}
	}
}
