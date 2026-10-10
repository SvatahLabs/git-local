package keymaterial

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// On a case-insensitive volume (macOS by default) a differently cased path
// into the checkout is the checkout; the key must still be refused there.
func TestKeyInCheckoutUnderAnotherCaseIsRefused(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "Workspace", "App")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	variant := filepath.Join(base, "workspace", "app")
	if _, err := os.Stat(variant); err != nil {
		t.Skip("case-sensitive volume")
	}
	for _, path := range []string{filepath.Join(variant, "file.key"), filepath.Join(variant, "nested", "..", "file.key")} {
		if err := Create(path, root); err == nil || !strings.Contains(err.Error(), "outside the repository checkout") {
			t.Fatalf("key at %s accepted inside the checkout: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "file.key")); !os.IsNotExist(err) {
		t.Fatalf("refused key was written into the checkout: %v", err)
	}
	outside := filepath.Join(base, "keys")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Create(filepath.Join(outside, "file.key"), root); err != nil {
		t.Fatalf("key outside the checkout: %v", err)
	}
	if inside, err := Inside(root, filepath.Join(strings.ToUpper(variant[:1])+variant[1:], "deeper", "missing.key")); err != nil || !inside {
		t.Fatalf("Inside for a missing descendant: %v %v", inside, err)
	}
}
