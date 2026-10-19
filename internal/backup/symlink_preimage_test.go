//go:build !windows

package backup

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/encryption"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/keymaterial"
	"github.com/SvatahLabs/git-local/internal/restore"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// TestSymlinkRestoreSurvivesMigrationAndBackupAdoption: a restore that
// replaced a symlink records the symlink's own permission bits, 0755 on macOS,
// in its preimage, while blob fingerprints use 0777. Encryption migration and
// backup adoption must still accept the completed restore journal.
func TestSymlinkRestoreSurvivesMigrationAndBackupAdoption(t *testing.T) {
	ctx := context.Background()
	source := testutil.New(t)
	source.Commit()
	r, err := gitexec.New(source.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(ctx, r, source.Root)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source.Root, "notes.link")
	if err := os.Symlink("saved-target", link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Root, ".env.local"), []byte("TOKEN=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, source.Root, true, "notes.link", ".env.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, source.Root, "symlink snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("unsaved-target", link); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("symlink permission bits: %o", info.Mode().Perm())
	}
	if result, err := restore.Execute(ctx, r, source.Root, w.WorkspaceID, "latest", []string{"notes.link"}, true); err != nil || result.TransactionID == "" {
		t.Fatalf("restore over a symlink: %+v %v", result, err)
	}
	key := filepath.Join(t.TempDir(), "files.key")
	if err := keymaterial.Create(key, source.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := encryption.Migrate(ctx, r, source.Root, key, []string{".env.local"}); err != nil {
		t.Fatalf("encryption migration after a symlink restore: %v", err)
	}
	backupKey, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	var ref string
	if err := configtx.ReadSelection(ctx, r, source.Root, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		ref = testRef(w.RepositoryID)
		if err := PackEncrypted(ctx, filepath.Join(w.Common, "gitlocal"), w.Admin, &archive, backupKey, ref); err != nil {
			return err
		}
		return recheck()
	}); err != nil {
		t.Fatal(err)
	}
	stage, err := DecryptStaged(ctx, bytes.NewReader(archive.Bytes()), backupKey, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, clone)
	cmd.Env = source.Env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh clone: %v %s", err, out)
	}
	if _, err := AdoptStaged(ctx, r, clone, stage, key); err != nil {
		t.Fatalf("backup adoption after a symlink restore: %v", err)
	}
	if target, err := history.Show(ctx, r, clone, "", "latest", "notes.link"); err != nil || string(target) != "saved-target" {
		t.Fatalf("adopted symlink snapshot: %q %v", target, err)
	}
}
