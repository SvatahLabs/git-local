package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/keymaterial"
	"github.com/SvatahLabs/git-local/internal/secureblob"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

func reopenCompletedSeedJournal(t *testing.T, common, id string) {
	t.Helper()
	meta := filepath.Join(common, "gitlocal", "transactions")
	completed := filepath.Join(meta, "completed", "seed-"+id+".json")
	pending := filepath.Join(meta, "seed-"+id+".json")
	data, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]any
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	journal["outcome"] = ""
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(completed, pending); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedSourceSeedsLinkedWorktreeAndSnapshotsThere(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	main, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, main.Root, main.Common, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(main.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, main.Root, main.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	if _, err := history.Commit(ctx, r, repo.Root, "encrypted source"); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed-sensitive", linked)
	seeded, err := Run(ctx, r, linked, main.WorkspaceID, nil)
	if err != nil || seeded.TransactionID == "" {
		t.Fatalf("encrypted source seed: %+v %v", seeded, err)
	}
	journalBytes, err := os.ReadFile(filepath.Join(main.Common, "gitlocal", "transactions", "completed", "seed-"+seeded.TransactionID+".json"))
	if err != nil || !bytes.Contains(journalBytes, []byte("plainDigest")) || bytes.Contains(journalBytes, []byte("plainBlob")) || bytes.Contains(journalBytes, []byte("TOKEN=private")) {
		t.Fatalf("sensitive seed journal contains unprotected content evidence: %v", err)
	}
	sourceSnapshot, err := history.Resolve(ctx, r, repo.Root, main.WorkspaceID, "latest")
	if err != nil {
		t.Fatal(err)
	}
	plainID, err := history.PlainBlobIDLocked(ctx, r, *main, sourceSnapshot, ".env.local")
	if err != nil || bytes.Contains(journalBytes, []byte(plainID)) {
		t.Fatalf("sensitive seed journal contains plaintext object ID: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(linked, ".env.local"))
	if err != nil || string(content) != "TOKEN=private\n" {
		t.Fatalf("seeded plaintext worktree file: %q %v", content, err)
	}
	reopenCompletedSeedJournal(t, main.Common, seeded.TransactionID)
	originalKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, linked, seeded.TransactionID, false); err == nil {
		t.Fatal("wrong key permitted encrypted seed resume")
	}
	if err := os.WriteFile(keyPath, originalKey, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, linked, seeded.TransactionID, false); err != nil {
		t.Fatalf("resume encrypted seed: %v", err)
	}
	if saved, err := history.Commit(ctx, r, linked, "encrypted destination"); err != nil || !saved.Changed {
		t.Fatalf("destination encrypted snapshot: %+v %v", saved, err)
	}
	linkedRollback := filepath.Join(t.TempDir(), "linked-rollback")
	repo.Git("worktree", "add", "-q", "-b", "seed-sensitive-rollback", linkedRollback)
	rolled, err := Run(ctx, r, linkedRollback, main.WorkspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopenCompletedSeedJournal(t, main.Common, rolled.TransactionID)
	// Moving the key away and back keeps its owner-private ACL on Windows,
	// which a new file written with os.WriteFile wouldn't get.
	aside := keyPath + ".aside"
	if err := os.Rename(keyPath, aside); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, linkedRollback, rolled.TransactionID, true); err == nil {
		t.Fatal("missing key permitted unverifiable encrypted seed rollback")
	}
	if err := os.Rename(aside, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, linkedRollback, rolled.TransactionID, true); err != nil {
		t.Fatalf("rollback encrypted seed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linkedRollback, ".env.local")); !os.IsNotExist(err) {
		t.Fatalf("rollback retained seeded file: %v", err)
	}
}

func TestRecoverValidatesReachableSourceSnapshot(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	main, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes", "a"), []byte("seeded"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "notes/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, repo.Root, "source"); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	result, err := Run(ctx, r, linked, main.WorkspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(main.Common, "gitlocal", "transactions")
	completed := filepath.Join(meta, "completed", "seed-"+result.TransactionID+".json")
	pending := filepath.Join(meta, "seed-"+result.TransactionID+".json")
	b, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]any
	if err := json.Unmarshal(b, &journal); err != nil {
		t.Fatal(err)
	}
	journal["outcome"] = ""
	b, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(completed, pending); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, linked, result.TransactionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(completed); err != nil {
		t.Fatal("recovery did not archive", err)
	}
}
