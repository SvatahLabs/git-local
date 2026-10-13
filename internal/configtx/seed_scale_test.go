package configtx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
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
