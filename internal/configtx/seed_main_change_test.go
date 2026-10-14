package configtx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// stoppedByMainChange seeds notes/a, notes/b and notes/c into a linked
// worktree and makes a lasting change to main (a commit) in the destination
// after the first file, which stops the seed; change then runs, before the
// recovery. It returns the worktree, the runner's recovery and the seed's ID.
func stoppedByMainChange(t *testing.T, format string, change func(repo *testutil.Repo, linked string)) (string, func(rollback bool) error) {
	t.Helper()
	ctx := context.Background()
	newRepo := testutil.New
	if format == "reftable" {
		newRepo = testutil.NewReftable
	}
	repo, r, main := fixtureFrom(t, newRepo(t))
	repo.Commit()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(out))
	planner := func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		files := []SeedFile{}
		for _, name := range []string{"notes/a", "notes/b", "notes/c"} {
			files = append(files, SeedFile{Path: name, Mode: "100644", Blob: blob})
		}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: blob, Selection: state, Files: files}, nil
	}
	result, err := seed(ctx, r, linked, planner, func(at string) error {
		if at == "seed-file-0" {
			repo.At(linked, "commit", "-q", "--allow-empty", "-m", "moved")
		}
		return nil
	})
	// No Git command holds the index or HEAD, so waiting wouldn't help.
	if err == nil || !strings.Contains(err.Error(), "main HEAD or index changed") || strings.Contains(err.Error(), "retry after Git operations finish") {
		t.Fatalf("seed after a commit in the destination: %v", err)
	}
	if change != nil {
		change(repo, linked)
	}
	return linked, func(rollback bool) error {
		_, err := RecoverSeed(ctx, r, linked, result.TransactionID, rollback)
		return err
	}
}

func seededFiles(t *testing.T, linked string) []string {
	t.Helper()
	var found []string
	for _, name := range []string{"a", "b", "c"} {
		if b, err := os.ReadFile(filepath.Join(linked, "notes", name)); err == nil && string(b) == "seeded" {
			found = append(found, name)
		} else if !os.IsNotExist(err) {
			t.Fatalf("notes/%s: %q %v", name, b, err)
		}
	}
	return found
}

// GL-125: a commit in the destination stops a seed, and afterwards both
// resume and rollback work: they check the seed's paths against main
// ownership as it is now, not against main as it was when the seed began.
// Before, both refused for as long as main differed, so only abandon worked.
func TestSeedStoppedByMainChangeResumesAndRollsBack(t *testing.T) {
	for _, format := range []string{"files", "reftable"} {
		for _, rollback := range []bool{false, true} {
			name := format + map[bool]string{false: "/resume", true: "/rollback"}[rollback]
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				linked, recover := stoppedByMainChange(t, format, nil)
				if got := seededFiles(t, linked); len(got) != 1 {
					t.Fatalf("files seeded before the stop: %v", got)
				}
				if err := recover(rollback); err != nil {
					t.Fatalf("recover (rollback %v) after a commit: %v", rollback, err)
				}
				want := 3
				if rollback {
					want = 0
				}
				if got := seededFiles(t, linked); len(got) != want {
					t.Fatalf("seeded files after recovery (rollback %v): %v", rollback, got)
				}
				if _, err := os.Lstat(filepath.Join(linked, "notes")); rollback && !os.IsNotExist(err) {
					t.Fatalf("rollback left notes: %v", err)
				}
			})
		}
	}
}

// GL-125: when main now owns a path the seed wrote, rollback (which would
// remove it) and resume both refuse, naming the path and abandon; when main
// owns only a path the seed's selection covers, resume refuses, naming
// rollback, and rollback works.
func TestSeedRecoveryRefusesPathsMainOwnsNow(t *testing.T) {
	t.Run("seeded", func(t *testing.T) {
		t.Parallel()
		linked, recover := stoppedByMainChange(t, "files", func(repo *testutil.Repo, linked string) {
			repo.At(linked, "add", "-f", "notes/a")
			repo.At(linked, "commit", "-q", "-m", "main takes notes/a")
		})
		for _, rollback := range []bool{true, false} {
			err := recover(rollback)
			if err == nil || !strings.Contains(err.Error(), `"notes/a"`) || !strings.Contains(err.Error(), "--abandon") || strings.Contains(err.Error(), "--rollback to") {
				t.Fatalf("recover (rollback %v) of a path main owns now: %v", rollback, err)
			}
		}
		if got := seededFiles(t, linked); len(got) != 1 {
			t.Fatalf("refused recovery changed the seeded files: %v", got)
		}
	})
	t.Run("selected", func(t *testing.T) {
		t.Parallel()
		linked, recover := stoppedByMainChange(t, "files", func(repo *testutil.Repo, linked string) {
			if err := os.WriteFile(filepath.Join(linked, "notes", "main.txt"), []byte("main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			repo.At(linked, "add", "-f", "notes/main.txt")
			repo.At(linked, "commit", "-q", "-m", "main takes notes/main.txt")
		})
		err := recover(false)
		if err == nil || !strings.Contains(err.Error(), `"notes/main.txt"`) || !strings.Contains(err.Error(), "--rollback") {
			t.Fatalf("resume of a selection main owns part of now: %v", err)
		}
		if err := recover(true); err != nil {
			t.Fatalf("rollback when main owns only a selected path: %v", err)
		}
		if got := seededFiles(t, linked); len(got) != 0 {
			t.Fatalf("rollback left seeded files: %v", got)
		}
	})
}
