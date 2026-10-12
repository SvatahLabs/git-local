package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestPackEncryptedIncludesPrivateStoreAndIdentity(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	g, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	w, err := workspace.Init(context.Background(), g, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("unique-secret-that-must-not-appear-in-ciphertext")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), secret, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(context.Background(), g, repo.Root, true, "notes.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(context.Background(), g, repo.Root, "backup source"); err != nil {
		t.Fatal(err)
	}
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	err = configtx.ReadSelection(context.Background(), g, repo.Root, func(current workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		if err := PackEncrypted(context.Background(), filepath.Join(current.Common, "gitlocal"), current.Admin, &encrypted, key, testRef(current.RepositoryID)); err != nil {
			return err
		}
		return recheck()
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.Bytes(), secret) {
		t.Fatal("encrypted archive exposed selected file bytes")
	}
	var plain bytes.Buffer
	if err := Decrypt(&plain, bytes.NewReader(encrypted.Bytes()), key, testRef(w.RepositoryID)); err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	reader := tar.NewReader(&plain)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			entries[header.Name], err = io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if !bytes.Contains(entries["identity.json"], []byte(w.WorkspaceID)) || !bytes.Contains(entries["state/repository.json"], []byte(w.RepositoryID)) || len(entries["state/history.git/config"]) == 0 {
		t.Fatalf("incomplete encrypted archive: %d entries", len(entries))
	}
	// The lock file is empty: check that it's absent, not that it's empty.
	if _, archived := entries["state/locks/repository.lock"]; archived || len(entries["state/repository.json"]) == 0 {
		t.Fatal("transient repository lock was archived")
	}
	stage, err := DecryptStaged(context.Background(), bytes.NewReader(encrypted.Bytes()), key, testRef(w.RepositoryID))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	if restored, err := os.ReadFile(filepath.Join(stage, "identity.json")); err != nil || !bytes.Equal(restored, entries["identity.json"]) {
		t.Fatalf("staged backup identity: %v %q", err, restored)
	}
	if restored, err := os.ReadFile(filepath.Join(stage, "state", "history.git", "config")); err != nil || !bytes.Equal(restored, entries["state/history.git/config"]) {
		t.Fatalf("staged backup history store: %v %q", err, restored)
	}
	validated, err := ValidateStaged(context.Background(), g, stage)
	if err != nil || validated.RepositoryID != w.RepositoryID || validated.WorkspaceID != w.WorkspaceID {
		t.Fatalf("staged backup validation: %+v %v", validated, err)
	}
	bad := bytes.Clone(encrypted.Bytes())
	bad[len(bad)-1] ^= 1
	if stage, err := DecryptStaged(context.Background(), bytes.NewReader(bad), key, testRef(w.RepositoryID)); err == nil || stage != "" {
		t.Fatalf("tampered backup produced a published stage: %q %v", stage, err)
	}
}

func TestPackEncryptedRejectsSymlinkedState(t *testing.T) {
	state := t.TempDir()
	admin := t.TempDir()
	if err := os.WriteFile(filepath.Join(admin, "gitlocal-workspace.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(state, "redirect")); err != nil {
		t.Skip(err)
	}
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := PackEncrypted(context.Background(), state, admin, io.Discard, key, codecRef); err == nil {
		t.Fatal("symlinked state was archived")
	}
}
