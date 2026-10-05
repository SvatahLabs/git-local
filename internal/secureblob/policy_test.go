package secureblob

import (
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/keymaterial"
	"gitlocal/internal/storage"
)

func TestPolicyKeyLifecycleAndPathRules(t *testing.T) {
	repo, common, keys := t.TempDir(), t.TempDir(), t.TempDir()
	keyFile := filepath.Join(keys, "sensitive.key")
	if err := keymaterial.Create(keyFile, repo); err != nil {
		t.Fatal(err)
	}
	rules := []PathRule{{Path: "notes/private", Kind: "directory"}, {Path: ".env.local", Kind: "file"}}
	policy, err := PolicyForKey(keyFile, repo, common, rules)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Sensitive[0].Path != ".env.local" || !policy.Covers("notes/private/token") || !policy.Covers(".env.local") || policy.Covers("notes/public/token") {
		t.Fatalf("bad canonical policy: %+v", policy)
	}
	if _, err := LoadKey(policy, repo, common); err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(keys, "wrong.key")
	if err := keymaterial.Create(wrong, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := RebindKeyPath(policy, wrong, repo, common); err == nil {
		t.Fatal("wrong key rebound to policy")
	}
	copyPath := filepath.Join(keys, "moved.key")
	content, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, content, 0600); err != nil {
		t.Fatal(err)
	}
	resolvedCopy, err := resolvedPath(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if rebound, err := RebindKeyPath(policy, copyPath, repo, common); err != nil || rebound.KeyFile != resolvedCopy {
		t.Fatalf("same key did not rebind: %+v %v", rebound, err)
	}
	if MissingKey(policy) {
		t.Fatal("existing key reported missing")
	}
}

func TestPolicyRejectsUnsafeLocationsAndRules(t *testing.T) {
	repo, common, keys := t.TempDir(), t.TempDir(), t.TempDir()
	keyFile := filepath.Join(keys, "key")
	if err := keymaterial.Create(keyFile, repo); err != nil {
		t.Fatal(err)
	}
	for _, rules := range [][]PathRule{
		{{Path: "notes", Kind: "directory"}, {Path: "notes/a", Kind: "file"}},
		{{Path: "A", Kind: "file"}, {Path: "a", Kind: "file"}},
		{{Path: ".git/config", Kind: "file"}},
		{{Path: "x", Kind: "other"}},
	} {
		if _, err := PolicyForKey(keyFile, repo, common, rules); err == nil {
			t.Fatalf("invalid rules accepted: %+v", rules)
		}
	}
	insideCommon := filepath.Join(common, "key")
	if err := os.WriteFile(insideCommon, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PolicyForKey(insideCommon, repo, common, []PathRule{{Path: "x", Kind: "file"}}); err == nil {
		t.Fatal("key inside common Git directory accepted")
	}
}

func TestPolicyStorageRoundTripAndWrongKeyRefusal(t *testing.T) {
	repo, common, keys := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(common, "gitlocal"), 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := storage.OpenDir(common)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if _, exists, err := ReadLocked(dir, repo, common); err != nil || exists {
		t.Fatalf("absent policy: %v %v", exists, err)
	}
	keyFile := filepath.Join(keys, "key")
	if err := keymaterial.Create(keyFile, repo); err != nil {
		t.Fatal(err)
	}
	policy, err := PolicyForKey(keyFile, repo, common, []PathRule{{Path: ".env.local", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteLocked(dir, policy, repo, common); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := ReadLocked(dir, repo, common)
	if err != nil || !exists || loaded.KeyFingerprint != policy.KeyFingerprint || !loaded.Covers(".env.local") {
		t.Fatalf("policy round trip: %+v %v %v", loaded, exists, err)
	}
	if err := os.WriteFile(keyFile, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocked(dir, policy, repo, common); err == nil {
		t.Fatal("wrong key accepted for policy publication")
	}
}
