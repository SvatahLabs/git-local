package restore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
)

func TestProtectedRestoreMixedDirectoryAndSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink restore is unavailable on Windows")
	}
	repo, r, w := fixture(t)
	write(t, repo.Root, "notes/public", "public saved\n")
	if err := os.Symlink("saved-target", filepath.Join(repo.Root, "notes", "private-link")); err != nil {
		t.Fatal(err)
	}
	track(t, r, repo.Root, true, "notes")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: "notes/private-link", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	source := save(t, r, repo.Root)
	write(t, repo.Root, "notes/public", "public changed\n")
	if err := os.Remove(filepath.Join(repo.Root, "notes", "private-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("changed-target", filepath.Join(repo.Root, "notes", "private-link")); err != nil {
		t.Fatal(err)
	}
	result, err := Execute(ctx, r, repo.Root, "", source, []string{"notes"}, true)
	if err != nil || !result.Changed {
		t.Fatalf("mixed protected restore: %+v %v", result, err)
	}
	public, err := os.ReadFile(filepath.Join(repo.Root, "notes", "public"))
	if err != nil || string(public) != "public saved\n" {
		t.Fatalf("public restore: %q %v", public, err)
	}
	target, err := os.Readlink(filepath.Join(repo.Root, "notes", "private-link"))
	if err != nil || target != "saved-target" {
		t.Fatalf("protected symlink restore: %q %v", target, err)
	}
	meta, err := openMeta(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	j, err := readJournal(meta, completedPath(result.TransactionID))
	if err != nil || j.SchemaVersion != 2 || len(j.Manifest.PostStored) != 2 {
		t.Fatalf("mixed recovery journal: %+v %v", j, err)
	}
	for i, op := range j.Manifest.Plan.Operations {
		if op.Path == "notes/public" && (op.Encrypted || j.Manifest.PostStored[i] != j.Manifest.Post[i].BlobID) {
			t.Fatal("public file stored incorrectly in mixed restore")
		}
		if op.Path == "notes/private-link" && (!op.Encrypted || j.Manifest.Post[i].KeyedDigest == "" || j.Manifest.Post[i].BlobID != "") {
			t.Fatal("protected symlink journal exposed unkeyed evidence")
		}
	}
}

func TestProtectedRestoreCreateRollsBackWithoutPreimage(t *testing.T) {
	t.Parallel()
	repo, r, w := fixture(t)
	write(t, repo.Root, ".env.local", "TOKEN=saved\n")
	track(t, r, repo.Root, true, ".env.local")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	source := save(t, r, repo.Root)
	remove(t, repo.Root, ".env.local")
	save(t, r, repo.Root)
	cut := func(at string) error {
		if at == "install:0" {
			return errors.New("injected interruption")
		}
		return nil
	}
	stopped, err := execute(ctx, r, repo.Root, "", source, []string{".env.local"}, false, cut)
	if err == nil || stopped.TransactionID == "" {
		t.Fatalf("protected creation did not stop: %+v %v", stopped, err)
	}
	if _, err := Recover(ctx, r, repo.Root, stopped.TransactionID, true); err != nil {
		t.Fatalf("rollback protected creation: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repo.Root, ".env.local")); !os.IsNotExist(err) {
		t.Fatalf("rollback retained created file: %v", err)
	}
}

func TestProtectedRestoreResumeAndRollback(t *testing.T) {
	t.Parallel()
	repo, r, w := fixture(t)
	write(t, repo.Root, ".env.local", "TOKEN=saved\n")
	track(t, r, repo.Root, true, ".env.local")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	source := save(t, r, repo.Root)
	write(t, repo.Root, ".env.local", "TOKEN=unsaved\n")
	cut := func(at string) error {
		if at == "install:0" {
			return errors.New("injected interruption")
		}
		return nil
	}
	stopped, err := execute(ctx, r, repo.Root, "", source, []string{".env.local"}, true, cut)
	if err == nil || stopped.TransactionID == "" {
		t.Fatalf("protected restore did not stop after installation: %+v %v", stopped, err)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, repo.Root, stopped.TransactionID, true); err == nil {
		t.Fatal("wrong key permitted protected rollback")
	}
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, repo.Root, stopped.TransactionID, true); err != nil {
		t.Fatalf("protected rollback: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(repo.Root, ".env.local"))
	if err != nil || string(content) != "TOKEN=unsaved\n" {
		t.Fatalf("rollback lost preimage: %q %v", content, err)
	}
	cut = func(at string) error {
		if at == "stage:0" {
			return errors.New("injected interruption")
		}
		return nil
	}
	stopped, err = execute(ctx, r, repo.Root, "", source, []string{".env.local"}, true, cut)
	if err == nil || stopped.TransactionID == "" {
		t.Fatalf("protected restore did not stop after staging: %+v %v", stopped, err)
	}
	if _, err := Recover(ctx, r, repo.Root, stopped.TransactionID, false); err != nil {
		t.Fatalf("protected resume: %v", err)
	}
	content, err = os.ReadFile(filepath.Join(repo.Root, ".env.local"))
	if err != nil || string(content) != "TOKEN=saved\n" {
		t.Fatalf("resume did not install source: %q %v", content, err)
	}
}

func TestEncryptedRecoveryBlobHasKeyedFingerprintWithoutPlaintextGitObject(t *testing.T) {
	t.Parallel()
	repo, r, w := fixture(t)
	content := append([]byte("TOKEN=private-value\n"), 0, 1, 2)
	writePath := filepath.Join(repo.Root, ".env.local")
	if err := os.WriteFile(writePath, content, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := storage.OpenDir(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	key := bytes.Repeat([]byte{7}, 32)
	identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Path: ".env.local", Mode: "100644"}
	cipherID, err := secureblob.EncryptGitBlob(ctx, r, w.Store, bytes.NewReader(content), key, identity)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.History(ctx, w.Store, nil, "cat-file", "blob", cipherID)
	if err != nil || bytes.Contains(raw, []byte("private-value")) {
		t.Fatalf("recovery object contains plaintext: %v", err)
	}
	worktree, err := inspectSensitiveFile(files, ".env.local", key, identity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := protectedBlobFingerprint(ctx, r, w.Store, cipherID, key, identity, 0600)
	if err != nil || got != protectedFingerprint(worktree) {
		t.Fatalf("keyed recovery fingerprint: got %+v, want %+v: %v", got, protectedFingerprint(worktree), err)
	}
	digest, size, err := encryptedBlobDigest(ctx, r, w.Store, cipherID, key, identity)
	if err != nil || digest != worktree.Digest || size != worktree.Size || bytes.Contains([]byte(worktree.Digest), []byte("private-value")) {
		t.Fatalf("keyed recovery evidence: %+v, digest %q, size %d: %v", worktree, digest, size, err)
	}
	identity.Path = "other"
	if _, err := protectedBlobFingerprint(ctx, r, w.Store, cipherID, key, identity, 0600); err == nil {
		t.Fatal("recovery ciphertext accepted for another path")
	}
}
