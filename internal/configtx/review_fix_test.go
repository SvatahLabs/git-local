package configtx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// seedLinked makes a linked worktree with a workspace, writes content to its
// private store and returns a planner seeding it at each of paths with mode.
func seedLinked(t *testing.T, content, mode string, paths ...string) (*testutil.Repo, *gitexec.Runner, string, SeedPlanner) {
	t.Helper()
	ctx := context.Background()
	repo, r, main := fixtureFrom(t, testutil.New(t))
	repo.Commit()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.History(ctx, w.Store, strings.NewReader(content), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(out))
	planner := func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		files := []SeedFile{}
		for _, p := range paths {
			files = append(files, SeedFile{Path: p, Mode: mode, Blob: blob})
		}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: blob, Selection: state, Files: files}, nil
	}
	return repo, r, linked, planner
}

// A plain symlink target over 1 MiB is refused like an encrypted one; before,
// io.Copy used the embedded bytes.Buffer's ReadFrom and skipped the limit.
func TestSeedRefusesPlainSymlinkTargetOver1MiB(t *testing.T) {
	t.Parallel()
	_, r, linked, planner := seedLinked(t, strings.Repeat("x", 1<<20+1), "120000", "notes/link")
	_, err := seed(context.Background(), r, linked, planner, nil)
	if err == nil || !strings.Contains(err.Error(), "seed symlink target exceeds 1 MiB") {
		t.Fatalf("seed of a symlink target over 1 MiB: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linked, "notes", "link")); !os.IsNotExist(err) {
		t.Fatalf("refused symlink was created: %v", err)
	}
}

// A seed that recorded its outcome but stopped before archiving the journal
// is archived by recovery even after main changed: archiving doesn't compare
// main with the seed's old evidence.
func TestSeedRecoveryArchivesCommittedSeedAfterMainChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, r, linked, planner := seedLinked(t, "seeded", "100644", "notes/a")
	result, err := seed(ctx, r, linked, planner, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := open(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	// Undo the archiving, as if the process stopped just before it.
	err = s.meta.Rename(seedCompletedPath(result.TransactionID), seedPath(result.TransactionID))
	s.close()
	if err != nil {
		t.Fatal(err)
	}
	repo.At(linked, "commit", "-q", "--allow-empty", "-m", "moved")
	if _, err := RecoverSeed(ctx, r, linked, result.TransactionID, false); err != nil {
		t.Fatalf("recover --resume of a committed seed after a main commit: %v", err)
	}
	s, err = open(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if _, err := s.meta.Lstat(seedCompletedPath(result.TransactionID)); err != nil {
		t.Fatalf("committed seed not archived: %v", err)
	}
	if _, err := s.meta.Lstat(seedPath(result.TransactionID)); !os.IsNotExist(err) {
		t.Fatalf("pending seed journal left: %v", err)
	}
}

// The refusal of a pending seed's recovery because main owns a path doesn't
// pass on the owned-path advice to untrack or exclude it, which the pending
// seed refuses; it names abandon (and rollback, when that works).
func TestSeedOwnedRefusalOmitsUntrackAdvice(t *testing.T) {
	t.Run("seeded", func(t *testing.T) {
		t.Parallel()
		_, recover := stoppedByMainChange(t, "files", func(repo *testutil.Repo, linked string) {
			repo.At(linked, "add", "-f", "notes/a")
			repo.At(linked, "commit", "-q", "-m", "main takes notes/a")
		})
		for _, rollback := range []bool{true, false} {
			err := recover(rollback)
			if err == nil || !strings.Contains(err.Error(), `"notes/a" is owned by the main index or HEAD`) || !strings.Contains(err.Error(), "--abandon") || strings.Contains(err.Error(), "untrack") {
				t.Fatalf("recover (rollback %v) of a path main owns now: %v", rollback, err)
			}
		}
	})
	t.Run("selected", func(t *testing.T) {
		t.Parallel()
		_, recover := stoppedByMainChange(t, "files", func(repo *testutil.Repo, linked string) {
			if err := os.WriteFile(filepath.Join(linked, "notes", "main.txt"), []byte("main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			repo.At(linked, "add", "-f", "notes/main.txt")
			repo.At(linked, "commit", "-q", "-m", "main takes notes/main.txt")
		})
		err := recover(false)
		if err == nil || !strings.Contains(err.Error(), `"notes/main.txt" is owned by the main index or HEAD`) || !strings.Contains(err.Error(), "--rollback") || !strings.Contains(err.Error(), "--abandon") || strings.Contains(err.Error(), "untrack") {
			t.Fatalf("resume of a selection main owns now: %v", err)
		}
	})
}
