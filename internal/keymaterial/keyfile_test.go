package keymaterial

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExternalOwnerPrivateKey(t *testing.T) {
	repo, keys := t.TempDir(), t.TempDir()
	path := filepath.Join(keys, "sensitive.key")
	if err := Create(path, repo); err != nil {
		t.Fatal(err)
	}
	if err := Create(path, repo); err == nil {
		t.Fatal("existing key was overwritten")
	}
	key, err := Load(path, repo)
	if err != nil || len(key) != 32 {
		t.Fatalf("load key: %d bytes, %v", len(key), err)
	}
	first, err := Fingerprint(key)
	if err != nil || len(first) != 64 {
		t.Fatalf("fingerprint: %q %v", first, err)
	}
	if second, err := Fingerprint(bytes.Repeat([]byte{1}, 32)); err != nil || second == first {
		t.Fatalf("fingerprints do not distinguish keys: %q %v", second, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path, repo); err == nil {
			t.Fatal("group-readable key was accepted")
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Create(filepath.Join(repo, "key"), repo); err == nil {
		t.Fatal("key inside checkout was created")
	}
	if err := os.Symlink(path, filepath.Join(keys, "link.key")); err == nil {
		if _, err := Load(filepath.Join(keys, "link.key"), repo); err == nil {
			t.Fatal("symlinked key was accepted")
		}
	}
}
