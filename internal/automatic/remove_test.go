package automatic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/seed"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestRemoveWorktreeSavesFilesAndRequiresExplicitForceForDirtyGitState(t *testing.T) {
	ctx := context.Background()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "notes.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, repo.Root, "main note"); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "remove-test", linked)
	seeded, err := seed.Run(ctx, r, linked, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	repo.Git("worktree", "lock", linked)
	if _, err := RemoveWorktree(ctx, r, repo.Root, linked, true); err == nil {
		t.Fatal("locked worktree was removed")
	}
	repo.Git("worktree", "unlock", linked)
	if err := os.WriteFile(filepath.Join(linked, "notes.local"), []byte("unsaved before removal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "app.txt"), []byte("dirty tracked file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := RemoveWorktree(ctx, r, repo.Root, linked, false)
	if err == nil || first.Removed || !first.SnapshotChanged {
		t.Fatalf("dirty worktree removal: %+v %v", first, err)
	}
	if _, err := os.Stat(linked); err != nil {
		t.Fatalf("Git removed dirty worktree without force: %v", err)
	}
	second, err := RemoveWorktree(ctx, r, repo.Root, linked, true)
	if err != nil || !second.Removed || !second.Retired || second.WorkspaceID != seeded.WorkspaceID {
		t.Fatalf("forced worktree removal: %+v %v", second, err)
	}
	entries, err := configtx.ListWorkspaces(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, entry := range entries {
		retained = retained || entry.ID == seeded.WorkspaceID && entry.State == "retained"
	}
	if !retained {
		t.Fatal("removed worktree history was not retained")
	}
	log, err := history.LogWorkspace(ctx, r, repo.Root, seeded.WorkspaceID, 10)
	if err != nil || len(log) == 0 || !strings.Contains(log[0].Message, "before worktree removal") {
		t.Fatalf("selected files were not saved before removal: %+v %v", log, err)
	}
}
