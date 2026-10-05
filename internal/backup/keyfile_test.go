package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExternalKeyFileLifecycle(t *testing.T) {
	repo := t.TempDir()
	keys := t.TempDir()
	keyPath := filepath.Join(keys, "backup.key")
	if err := CreateKeyFile(keyPath, repo); err != nil {
		t.Fatal(err)
	}
	if err := CreateKeyFile(keyPath, repo); err == nil {
		t.Fatal("existing key was overwritten")
	}
	key, err := LoadKeyFile(keyPath, repo)
	if err != nil || len(key) != 32 {
		t.Fatal(len(key), err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(keyPath, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyFile(keyPath, repo); err == nil {
			t.Fatal("group-readable key was accepted")
		}
		if err := os.Chmod(keyPath, 0600); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(repo, "key")
	if err := CreateKeyFile(inside, repo); err == nil {
		t.Fatal("key inside checkout was created")
	}
	if err := os.Symlink(keyPath, filepath.Join(keys, "link.key")); err == nil {
		if _, err := LoadKeyFile(filepath.Join(keys, "link.key"), repo); err == nil {
			t.Fatal("symlinked key was accepted")
		}
	}
}
