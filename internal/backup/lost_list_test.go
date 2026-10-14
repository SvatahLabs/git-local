package backup

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/encryption"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/keymaterial"
	"gitlocal/internal/seed"
	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// GL-124: restoring an encrypted backup whose completed seed has no file list
// (an earlier release pushed it after the list was lost) names the remedy in
// the repository the backup came from. Before, it said to put the list back
// or run git local recover <id> --abandon, which doesn't apply here.
func TestAdoptNamesSourceRemedyForLostSeedList(t *testing.T) {
	t.Parallel()
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
	if err := os.WriteFile(filepath.Join(source.Root, ".env.local"), []byte("TOKEN=backup\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, source.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "files.key")
	if err := keymaterial.Create(key, source.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := encryption.EnableFresh(ctx, r, source.Root, key, []string{".env.local"}); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, source.Root, "encrypted snapshot"); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	source.Git("worktree", "add", "-q", "-b", "backup-seed", linked)
	seeded, err := seed.Run(ctx, r, linked, w.WorkspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	testutil.SeedFilesBeside(t, filepath.Join(w.Common, "gitlocal"), seeded.TransactionID, true)
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
	// What an earlier release pushed: the journal without its list.
	removed := false
	err = filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == seeded.TransactionID+".json" && filepath.Base(filepath.Dir(p)) == "seed-files" {
			removed = true
			return os.Remove(p)
		}
		return err
	})
	if err != nil || !removed {
		t.Fatalf("remove the staged seed list: %v %v", removed, err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, clone)
	cmd.Env = source.Env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	_, err = AdoptStaged(ctx, r, clone, stage, key)
	if err == nil || !strings.Contains(err.Error(), "in the repository the backup came from, run git local recover "+seeded.TransactionID+" --abandon") || strings.Contains(err.Error(), "put it back") {
		t.Fatalf("restore of a backup whose seed list is lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "gitlocal")); !os.IsNotExist(err) {
		t.Fatalf("refused restore published private state: %v", err)
	}
}
