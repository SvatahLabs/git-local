package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// GL-129 review: backup restore publishes the private state and the main
// identity, so it waits for the identity lock that git local deinit holds
// while it removes the identities that don't belong to the live state.
// Before, a restore that ran meanwhile published an identity deinit then
// removed. On POSIX deinit removes the lock file while it holds it, so the
// restore stops and publishes nothing; once the lock is free a restore, and
// a recovery of an interrupted one, finishes.
func TestRestoreWaitsForIdentityLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't remove an open lock file")
	}
	ctx := context.Background()
	source := testutil.New(t)
	source.Commit()
	g, err := gitexec.New(source.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := workspace.Init(ctx, g, source.Root); err != nil {
		t.Fatal(err)
	}
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	var ref string
	if err := configtx.ReadSelection(ctx, g, source.Root, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		ref = testRef(w.RepositoryID)
		if err := PackEncrypted(ctx, filepath.Join(w.Common, "gitlocal"), w.Admin, &encrypted, key, ref); err != nil {
			return err
		}
		return recheck()
	}); err != nil {
		t.Fatal(err)
	}
	stage, err := DecryptStaged(ctx, bytes.NewReader(encrypted.Bytes()), key, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, clone)
	cmd.Env = source.Env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, output)
	}
	common := filepath.Join(clone, ".git")
	d, err := storage.OpenDir(common)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// waits runs restore while the test holds the identity lock, as deinit
	// does, then removes the lock file and releases it, as deinit does last.
	waits := func(what string, restore func() error, published ...string) {
		t.Helper()
		held, err := d.Lock(ctx, workspace.IdentityLock, false)
		if err != nil {
			t.Fatal(err)
		}
		locking := make(chan struct{}, 1)
		identityLocking = func() {
			select {
			case locking <- struct{}{}:
			default:
			}
		}
		defer func() { identityLocking = func() {} }()
		done := make(chan error, 1)
		go func() { done <- restore() }()
		select {
		case err := <-done:
			held.Close()
			t.Fatalf("%s didn't reach the identity lock: %v", what, err)
		case <-locking:
		}
		// The lock is taken right after the hook; give it time to block.
		select {
		case err := <-done:
			held.Close()
			t.Fatalf("%s did not wait for the identity lock: %v", what, err)
		case <-time.After(500 * time.Millisecond):
		}
		for _, name := range published {
			if _, err := os.Lstat(filepath.Join(common, name)); !os.IsNotExist(err) {
				t.Errorf("%s published %s under deinit's identity lock: %v", what, name, err)
			}
		}
		if err := os.Remove(filepath.Join(common, workspace.IdentityLock)); err != nil {
			t.Fatal(err)
		}
		held.Close()
		if err := <-done; !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("%s after the identity lock was removed: %v", what, err)
		}
		for _, name := range published {
			if _, err := os.Lstat(filepath.Join(common, name)); !os.IsNotExist(err) {
				t.Fatalf("%s that stopped published %s: %v", what, name, err)
			}
		}
	}
	waits("restore", func() error {
		_, err := AdoptStaged(ctx, g, clone, stage)
		return err
	}, "gitlocal", "gitlocal-workspace.json", restoreJournalName)
	// A restore interrupted once its journal is written, before it publishes
	// anything.
	if _, err := adoptStaged(ctx, g, clone, stage, func(step string) error {
		if step == "journal" {
			return errors.New("simulated interruption")
		}
		return nil
	}); err == nil {
		t.Fatal("restore interruption was ignored")
	}
	waits("recovery", func() error {
		_, err := RecoverRestore(ctx, g, clone)
		return err
	}, "gitlocal", "gitlocal-workspace.json")
	if _, err := RecoverRestore(ctx, g, clone); err != nil {
		t.Fatalf("recovery once the identity lock is free: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(common, "gitlocal-workspace.json")); err != nil {
		t.Fatalf("recovery published no identity: %v", err)
	}
}
