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
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

var subdirectorySecret = []byte("TOKEN=subdirectory-private-value\n")

// trackedDirectoryFixture selects config/ as one directory rule, with the
// sensitive files one level below it in config/secrets/.
func trackedDirectoryFixture(t *testing.T) (*gitexec.Runner, *workspace.Context, string, string) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ctx := context.Background()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo.Root, "config", "secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "config", "secrets", "token"), subdirectorySecret, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "config", "app.txt"), []byte("public\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "config"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	return r, w, repo.Root, keyPath
}

func checkSubdirectoryEncrypted(t *testing.T, r *gitexec.Runner, w *workspace.Context, root string) {
	t.Helper()
	ctx := context.Background()
	snapshot, err := history.Resolve(ctx, r, root, w.WorkspaceID, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Metadata.EncryptedDigests["config/secrets/token"] == "" {
		t.Fatalf("file below the sensitive directory is not recorded as encrypted: %+v", snapshot.Metadata.EncryptedDigests)
	}
	raw, err := r.History(ctx, w.Store, nil, "cat-file", "blob", snapshot.Files["config/secrets/token"].ID)
	if err != nil || bytes.Contains(raw, subdirectorySecret) {
		t.Fatalf("file below the sensitive directory is plaintext in the private store: %v", err)
	}
}

func TestEnableFreshEncryptsSubdirectoryOfTrackedDirectory(t *testing.T) {
	r, w, root, keyPath := trackedDirectoryFixture(t)
	ctx := context.Background()
	policy, err := EnableFresh(ctx, r, root, keyPath, []string{"config/secrets"})
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Covers("config/secrets/token") || policy.Covers("config/app.txt") {
		t.Fatalf("policy rules: %+v", policy.Sensitive)
	}
	if _, err := history.Commit(ctx, r, root, "first"); err != nil {
		t.Fatal(err)
	}
	checkSubdirectoryEncrypted(t, r, w, root)
}

func TestMigrateEncryptsSubdirectoryOfTrackedDirectory(t *testing.T) {
	r, w, root, keyPath := trackedDirectoryFixture(t)
	ctx := context.Background()
	if _, err := history.Commit(ctx, r, root, "plaintext"); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, r, root, keyPath, []string{"config/secrets"}); err != nil {
		t.Fatal(err)
	}
	checkSubdirectoryEncrypted(t, r, w, root)
}

func TestSensitivePathMustExistWithItsOnDiskSpelling(t *testing.T) {
	r, _, root, keyPath := trackedDirectoryFixture(t)
	ctx := context.Background()
	for _, path := range []string{"config/missing", "config/Secrets", "config/secrets/Token"} {
		if _, err := EnableFresh(ctx, r, root, keyPath, []string{path}); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("enable accepted %q: %v", path, err)
		}
	}
	if _, err := history.Commit(ctx, r, root, "plaintext"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"config/missing", "config/Secrets"} {
		if _, err := Migrate(ctx, r, root, keyPath, []string{path}); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("migrate accepted %q: %v", path, err)
		}
	}
}
