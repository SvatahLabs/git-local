package configtx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// ownedSeed prepares a linked worktree whose main index holds owned paths,
// and a planner seeding n files into it; run seeds it and checks every file.
func ownedSeed(t *testing.T, n, owned int) (r *gitexec.Runner, run func()) {
	t.Helper()
	repo, r, main := fixture(t)
	repo.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	app := strings.TrimSpace(string(repo.At(linked, "hash-object", "-w", "app.txt")))
	args := []string{"update-index", "--add"}
	for i := 0; i < owned; i++ {
		args = append(args, "--cacheinfo", fmt.Sprintf("100644,%s,main/file-%05d", app, i))
	}
	repo.At(linked, args...)
	blob, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(blob))
	files := make([]SeedFile, n)
	for i := range files {
		files[i] = SeedFile{Path: fmt.Sprintf("notes/file-%04d", i), Mode: "100644", Blob: id}
	}
	planner := func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: id, Selection: state, Files: append([]SeedFile{}, files...)}, nil
	}
	return r, func() {
		t.Helper()
		if _, err := Seed(ctx, r, linked, planner); err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if data, err := os.ReadFile(filepath.Join(linked, filepath.FromSlash(file.Path))); err != nil || string(data) != "seeded" {
				t.Fatalf("%s: %q %v", file.Path, data, err)
			}
		}
	}
}

// Seeding checks every staging path against main ownership with one fold of
// each owned path. Before, each stage was checked against every owned path.
// Not parallel: it replaces selection.FoldOwned.
func TestSeedOwnershipChecksDontGrowWithMainIndex(t *testing.T) {
	const owned, n = 3000, 20
	_, run := ownedSeed(t, n, owned)
	folds := 0
	selection.FoldOwned = func(p string) string { folds++; return storage.AliasKey(p) }
	defer func() { selection.FoldOwned = storage.AliasKey }()
	run()
	t.Logf("alias folds seeding %d files with %d owned paths: %d", n, owned, folds)
	if folds > 6*owned {
		t.Fatalf("seeding %d files made %d ownership folds against %d owned paths", n, folds, owned)
	}
}

// seedProcesses seeds n files into a worktree whose main index holds owned
// paths and returns the Git processes the seed ran.
func seedProcesses(t *testing.T, n, owned int) uint64 {
	t.Helper()
	r, run := ownedSeed(t, n, owned)
	before := r.SubprocessCount()
	run()
	return r.SubprocessCount() - before
}

// Before each file a seed stats the files Git replaces to move HEAD or change
// the index, and lists the main index again only when one changed; one
// cat-file process streams every blob. So the Git processes a seed runs don't
// grow with its files. Before, each file read HEAD, listed the whole main
// index and read its blob: five processes with the runner's configuration
// checks.
func TestSeedListsMainIndexOnlyWhenItChanged(t *testing.T) {
	t.Parallel()
	const owned = 3000
	small, large := seedProcesses(t, 10, owned), seedProcesses(t, 40, owned)
	t.Logf("Git processes seeding with %d owned paths: 10 files %d, 40 files %d", owned, small, large)
	if large-small > 10 {
		t.Fatalf("30 more seeded files ran %d more Git processes", large-small)
	}
}

// Between full listings a seed still stops when main ownership changes: a
// path taken into the main index or a new HEAD stops it before the next file,
// and that file isn't written. An index rewritten with the same entries is
// only listed again. Also with the reftable backend, which keeps a linked
// worktree's HEAD in the worktree's own stack.
func TestSeedDetectsOwnershipChangesBetweenListings(t *testing.T) {
	for _, format := range []string{"files", "reftable"} {
		for _, kind := range []string{"owned", "head", "update-ref", "detached-update-ref", "index-rewritten"} {
			t.Run(format+"/"+kind, func(t *testing.T) {
				t.Parallel()
				seedDetectsOwnershipChange(t, format, kind)
			})
		}
	}
}

func seedDetectsOwnershipChange(t *testing.T, format, kind string) {
	newRepo := testutil.New
	if format == "reftable" {
		newRepo = testutil.NewReftable
	}
	repo, r, main := fixtureFrom(t, newRepo(t))
	repo.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	if kind == "detached-update-ref" {
		repo.Git("worktree", "add", "-q", "--detach", linked)
	} else {
		repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	}
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(blob))
	names := []string{"notes/a", "notes/b", "notes/c"}
	planner := func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		files := []SeedFile{}
		for _, name := range names {
			files = append(files, SeedFile{Path: name, Mode: "100644", Blob: id})
		}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: id, Selection: state, Files: files}, nil
	}
	_, err = seed(ctx, r, linked, planner, func(at string) error {
		if at != "seed-file-0" {
			return nil
		}
		switch kind {
		case "owned":
			app := strings.TrimSpace(string(repo.At(linked, "hash-object", "-w", "app.txt")))
			repo.At(linked, "update-index", "--add", "--cacheinfo", "100644,"+app+",notes/b")
		case "head":
			repo.At(linked, "commit", "-q", "--allow-empty", "-m", "moved")
		case "update-ref":
			// Moves HEAD without touching the index.
			repo.At(linked, "update-ref", "HEAD", strings.TrimSpace(string(repo.At(linked, "commit-tree", "HEAD^{tree}", "-m", "moved"))))
		case "detached-update-ref":
			// Moves a detached HEAD without touching the index; the
			// reftable backend keeps it in the worktree's own stack.
			repo.At(linked, "update-ref", "--no-deref", "HEAD", strings.TrimSpace(string(repo.At(linked, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "moved"))))
		case "index-rewritten":
			index := filepath.Join(w.Admin, "index")
			b, e := os.ReadFile(index)
			if e != nil {
				return e
			}
			if e = os.WriteFile(index+".new", b, 0644); e != nil {
				return e
			}
			return os.Rename(index+".new", index)
		}
		return nil
	})
	if kind == "index-rewritten" {
		if err != nil {
			t.Fatalf("seed after an unchanged index rewrite: %v", err)
		}
		for _, name := range names {
			if b, e := os.ReadFile(filepath.Join(linked, filepath.FromSlash(name))); e != nil || string(b) != "seeded" {
				t.Fatalf("%s: %q %v", name, b, e)
			}
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "main HEAD or index changed") {
		t.Fatalf("seed accepted a main ownership change: %v", err)
	}
	if b, e := os.ReadFile(filepath.Join(linked, "notes", "a")); e != nil || string(b) != "seeded" {
		t.Fatalf("notes/a: %q %v", b, e)
	}
	if _, e := os.Lstat(filepath.Join(linked, "notes", "b")); !os.IsNotExist(e) {
		t.Fatalf("seed wrote notes/b after main ownership changed: %v", e)
	}
}
