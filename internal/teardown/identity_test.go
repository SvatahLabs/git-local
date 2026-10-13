package teardown

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// GL-118: git local init in another worktree after deinit set the private
// state aside builds new state; deinit's last step, which removes the
// identity files it listed, keeps that worktree's new identity. Not parallel: it
// replaces beforeRemoveIdentities.
func TestDeinitKeepsIdentityOfInitThatRacedIt(t *testing.T) {
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
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "linked", linked)
	var raced *workspace.Context
	beforeRemoveIdentities = func() {
		// Another worktree's init succeeds once the old identities it checks
		// are gone, here as if deinit had already removed the main one.
		if err := os.Remove(filepath.Join(repo.Root, ".git", "gitlocal-workspace.json")); err != nil {
			t.Error(err)
		}
		if raced, err = workspace.Init(ctx, r, linked); err != nil {
			t.Errorf("init racing deinit: %v", err)
		}
	}
	t.Cleanup(func() { beforeRemoveIdentities = func() {} })
	result, err := Deinit(ctx, r, repo.Root, false)
	if err != nil || result.Archive == "" {
		t.Fatalf("deinit: %+v %v", result, err)
	}
	if raced == nil {
		t.Fatal("no init raced deinit")
	}
	var id workspace.Identity
	data, err := os.ReadFile(filepath.Join(raced.Admin, "gitlocal-workspace.json"))
	if err == nil {
		err = json.Unmarshal(data, &id)
	}
	if err != nil || id.WorkspaceID != raced.WorkspaceID || id.RepositoryID != raced.RepositoryID {
		t.Fatalf("deinit removed the identity of the init that raced it: %+v %v", id, err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, ".git", "gitlocal-workspace.json")); !os.IsNotExist(err) {
		t.Fatalf("deinit kept the removed state's identity: %v", err)
	}
}
