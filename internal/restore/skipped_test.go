package restore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
)

func TestRestoreOfSkippedPathWritesNoNewPlaintext(t *testing.T) {
	repo, r, w := fixture(t)
	write(t, repo.Root, "notes/Secret.txt", "TOKEN=saved\n")
	write(t, repo.Root, "notes/plain.txt", "plain\n")
	track(t, r, repo.Root, true, "notes")
	// An earlier release saved it as plaintext, then recorded the policy
	// under another spelling, so snapshots now skip the file.
	source := save(t, r, repo.Root)
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: "notes/secret.txt", Kind: "file"}})
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
	write(t, repo.Root, "notes/Secret.txt", "TOKEN=unsaved-local-change\n")
	if _, err := Execute(ctx, r, repo.Root, "", source, []string{"notes/Secret.txt"}, true); !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "can't be kept as a recovery copy") {
		t.Fatalf("restore over an unsaved skipped file was not refused as a skipped path: %v", err)
	}
	objects, err := r.History(ctx, w.Store, nil, "cat-file", "--batch-all-objects", "--batch", "--unordered")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(objects, []byte("unsaved-local-change")) {
		t.Fatal("restore wrote the skipped file's plaintext into the private store")
	}
	if data, err := os.ReadFile(filepath.Join(repo.Root, "notes/Secret.txt")); err != nil || string(data) != "TOKEN=unsaved-local-change\n" {
		t.Fatalf("local change not kept: %q %v", data, err)
	}
	// The last saved version is still restorable once nothing would be lost.
	remove(t, repo.Root, "notes/Secret.txt")
	if _, err := Execute(ctx, r, repo.Root, "", source, []string{"notes/Secret.txt"}, false); err != nil {
		t.Fatalf("restore of the last saved version: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(repo.Root, "notes/Secret.txt")); err != nil || string(data) != "TOKEN=saved\n" {
		t.Fatalf("restored content: %q %v", data, err)
	}
}
