package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/encryption"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/keymaterial"
	"github.com/SvatahLabs/git-local/internal/restore"
	"github.com/SvatahLabs/git-local/internal/seed"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// A large seed keeps its file list beside the journal (GL-111); backups carry
// and verify both forms.
func TestAdoptEncryptedHistoryWithReboundFileKeyAndRecovery(t *testing.T) {
	for _, beside := range []bool{false, true} {
		t.Run(fmt.Sprintf("seed-files-beside=%v", beside), func(t *testing.T) {
			adoptEncryptedHistoryWithReboundFileKeyAndRecovery(t, beside)
		})
	}
}

func adoptEncryptedHistoryWithReboundFileKeyAndRecovery(t *testing.T, beside bool) {
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
	secret := []byte("TOKEN=encrypted-backup\n")
	file := filepath.Join(source.Root, ".env.local")
	if err := os.WriteFile(file, secret, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, source.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	oldKey := filepath.Join(t.TempDir(), "old-files.key")
	if err := keymaterial.Create(oldKey, source.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := encryption.EnableFresh(ctx, r, source.Root, oldKey, []string{".env.local"}); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, source.Root, "encrypted backup snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("TOKEN=unsaved-before-restore\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := restore.Execute(ctx, r, source.Root, w.WorkspaceID, "latest", []string{".env.local"}, true)
	if err != nil || recovered.TransactionID == "" {
		t.Fatalf("completed encrypted restore: %+v %v", recovered, err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	source.Git("worktree", "add", "-q", "-b", "backup-seed", linked)
	seeded, err := seed.Run(ctx, r, linked, w.WorkspaceID, nil)
	if err != nil || seeded.TransactionID == "" {
		t.Fatalf("completed encrypted seed: %+v %v", seeded, err)
	}
	if beside {
		testutil.SeedFilesBeside(t, filepath.Join(w.Common, "gitlocal"), seeded.TransactionID, true)
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
	if _, err := ValidateStaged(ctx, r, stage); err != nil {
		t.Fatalf("encrypted backup stage rejected: %v", err)
	}
	newKey := filepath.Join(t.TempDir(), "new-files.key")
	contents, err := os.ReadFile(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newKey, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldKey); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, clone)
	cmd.Env = source.Env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh clone: %v %s", err, out)
	}
	if _, err := AdoptStaged(ctx, r, clone, stage); err == nil || !strings.Contains(err.Error(), "--file-key") {
		t.Fatalf("missing relocated file key accepted: %v", err)
	}
	wrongKey := filepath.Join(t.TempDir(), "wrong-files.key")
	if err := keymaterial.Create(wrongKey, clone); err != nil {
		t.Fatal(err)
	}
	if _, err := AdoptStaged(ctx, r, clone, stage, wrongKey); err == nil {
		t.Fatal("wrong sensitive-file key accepted")
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "gitlocal")); !os.IsNotExist(err) {
		t.Fatalf("failed adoption published private state: %v", err)
	}
	_, err = adoptStaged(ctx, r, clone, stage, func(at string) error {
		if at == "journal" {
			return errors.New("injected interruption")
		}
		return nil
	}, newKey)
	if err == nil {
		t.Fatal("adoption interruption did not stop")
	}
	parkedKey := newKey + ".parked"
	if err := os.Rename(newKey, parkedKey); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverRestore(ctx, r, clone); err == nil {
		t.Fatal("backup recovery published without the sensitive-file key")
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "gitlocal-workspace.json")); !os.IsNotExist(err) {
		t.Fatalf("failed recovery published workspace identity: %v", err)
	}
	if err := os.Rename(parkedKey, newKey); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverRestore(ctx, r, clone); err != nil {
		t.Fatalf("resume encrypted backup adoption: %v", err)
	}
	if _, err := workspace.Init(ctx, r, clone); err != nil {
		t.Fatalf("restored encrypted workspace: %v", err)
	}
	shown, err := history.Show(ctx, r, clone, w.WorkspaceID, "latest", ".env.local")
	if err != nil || !bytes.Equal(shown, secret) {
		t.Fatalf("restored encrypted snapshot: %q %v", shown, err)
	}
	if err := configtx.ReadDiagnostic(ctx, r, clone, func(d configtx.DiagnosticState) error {
		return restore.InspectJournal(ctx, r, d.Workspace, d.Selection, d.ConfigDigest, recovered.TransactionID, true, d.Sources)
	}); err != nil {
		t.Fatalf("restored recovery anchor: %v", err)
	}
}
