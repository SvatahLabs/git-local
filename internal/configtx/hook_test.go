package configtx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestHookChecksSelectionsFromOtherActiveWorkspaces(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "linked", linked)
	if _, err := workspace.Init(ctx, r, linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(linked, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "notes", "secret"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, r, linked, true, "notes/secret"); err != nil {
		t.Fatal(err)
	}
	conflicts, err := CheckStagedPaths(ctx, r, repo.Root, []string{"app.txt", "notes/secret", "NOTES/SECRET"})
	if err != nil || len(conflicts) != 2 || conflicts[0].Path != "notes/secret" || conflicts[1].Path != "NOTES/SECRET" {
		t.Fatalf("cross-workspace guard: %+v, %v", conflicts, err)
	}
}
