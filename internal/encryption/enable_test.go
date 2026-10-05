package encryption

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestEnableFreshEncryptsFirstSnapshotAndRefusesExistingHistory(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("TOKEN=private-value\n")
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), secret, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := EnableFresh(ctx, r, repo.Root, keyPath, []string{".env.local"})
	if err != nil || !policy.Covers(".env.local") {
		t.Fatalf("enable empty history: %+v %v", policy, err)
	}
	if _, err := EnableFresh(ctx, r, repo.Root, keyPath, []string{".env.local"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("duplicate policy enable: %v", err)
	}
	saved, err := history.Commit(ctx, r, repo.Root, "first encrypted snapshot")
	if err != nil || !saved.Changed {
		t.Fatalf("first encrypted snapshot: %+v %v", saved, err)
	}
	snapshot, err := history.Resolve(ctx, r, repo.Root, w.WorkspaceID, "latest")
	if err != nil || snapshot.Metadata.EncryptedDigests[".env.local"] == "" {
		t.Fatalf("encrypted snapshot metadata: %+v %v", snapshot.Metadata, err)
	}
	raw, err := r.History(ctx, w.Store, nil, "cat-file", "blob", snapshot.Files[".env.local"].ID)
	if err != nil || bytes.Contains(raw, secret) {
		t.Fatalf("sensitive snapshot stored plaintext: %v", err)
	}
}

func TestEnableFreshRejectsPlaintextHistoryWithoutPublishingPolicy(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=old-plaintext\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, repo.Root, "plaintext snapshot"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableFresh(ctx, r, repo.Root, keyPath, []string{".env.local"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("plaintext history accepted without migration: %v", err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	if _, exists, err := secureblob.ReadLocked(common, w.Root, w.Common); err != nil || exists {
		t.Fatalf("failed enable published policy: exists=%t error=%v", exists, err)
	}
}

func TestEnableFreshRejectsUnreachablePrivateObjects(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.History(ctx, w.Store, bytes.NewReader([]byte("orphan plaintext")), "hash-object", "-w", "--stdin"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableFresh(ctx, r, repo.Root, keyPath, []string{".env.local"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("unreachable plaintext object accepted: %v", err)
	}
}

func TestEnableFreshRejectsKeyInsideLinkedWorktree(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "key-location-test", linked)
	keyPath := filepath.Join(linked, "sensitive.key")
	if err := Keygen(ctx, r, repo.Root, keyPath); err == nil {
		t.Fatal("keygen wrote a key inside linked worktree")
	}
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableFresh(ctx, r, repo.Root, keyPath, []string{".env.local"}); err == nil {
		t.Fatal("key inside linked worktree accepted")
	}
}
