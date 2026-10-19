package teardown

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
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
	result, err := Deinit(ctx, r, repo.Root, false, false)
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

// GL-129: a deinit rerun after the private state was set aside finds no live
// state, so every identity it then reaches is removed. An init in another
// worktree that builds a new state and publishes its identity meanwhile must
// either finish before deinit lists what belongs to the live state, or wait
// for deinit and refuse; before, it published its identity without waiting
// and deinit deleted it, leaving a state no worktree belongs to. Not
// parallel: it replaces identitiesListed.
func TestDeinitRerunKeepsIdentityOfInitThatRacedIt(t *testing.T) {
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
	// A deinit interrupted right after setting the state aside.
	if err := os.Rename(filepath.Join(repo.Root, ".git", "gitlocal"), filepath.Join(repo.Root, ".git", "gitlocal-removed-interrupted")); err != nil {
		t.Fatal(err)
	}
	var raced *workspace.Context
	var initErr error
	done := make(chan struct{})
	identitiesListed = func() {
		// The rerun has removed the main worktree's old identity, so an
		// init in the linked worktree no longer refuses.
		if err := os.Remove(filepath.Join(repo.Root, ".git", "gitlocal-workspace.json")); err != nil {
			t.Error(err)
		}
		go func() {
			defer close(done)
			raced, initErr = workspace.Init(ctx, r, linked)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	t.Cleanup(func() { identitiesListed = func() {} })
	if _, err := Deinit(ctx, r, repo.Root, false, false); err != nil {
		t.Fatalf("deinit rerun: %v", err)
	}
	<-done
	if initErr != nil {
		if !errors.Is(initErr, storage.ErrConflict) {
			t.Fatalf("init racing the deinit rerun: %v", initErr)
		}
		if _, err := os.Lstat(filepath.Join(repo.Root, ".git", "worktrees", "linked", "gitlocal-workspace.json")); !os.IsNotExist(err) {
			t.Fatalf("init refused but left an identity: %v", err)
		}
		return
	}
	var id workspace.Identity
	data, err := os.ReadFile(filepath.Join(raced.Admin, "gitlocal-workspace.json"))
	if err == nil {
		err = json.Unmarshal(data, &id)
	}
	if err != nil || id.WorkspaceID != raced.WorkspaceID || id.RepositoryID != raced.RepositoryID {
		t.Fatalf("deinit removed the identity of the init that raced it: %+v %v", id, err)
	}
}
