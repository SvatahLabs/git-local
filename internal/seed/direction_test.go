package seed

import (
	"context"
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

func TestFailedSeedRollbackLeavesResumeAvailable(t *testing.T) {
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
	repo.Git("worktree", "add", "-q", "-b", "seed-direction", linked)
	seeded, err := Run(ctx, r, linked, main.WorkspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopenCompletedSeedJournal(t, main.Common, seeded.TransactionID)
	hidden := keyPath + ".away"
	if err := os.Rename(keyPath, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, linked, seeded.TransactionID, true); err == nil {
		t.Fatal("rollback succeeded without the key")
	}
	if err := os.Rename(hidden, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, linked, seeded.TransactionID, false); err != nil {
		t.Fatalf("resume after a refused rollback: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(linked, ".env.local"))
	if err != nil || string(content) != "TOKEN=private\n" {
		t.Fatalf("seeded file: %q %v", content, err)
	}
}
