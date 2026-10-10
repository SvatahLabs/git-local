package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/storage"
)

// A crash can leave temporary files from atomic writes and the stores of an
// encryption migration killed before its journal. Push skips the former and
// refuses the latter, so every pushed archive restores.
func TestPushSkipsTempLeftoversAndRefusesUnknownState(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	source := newBoundSource(t, remote, "saved\n")
	ctx := context.Background()
	source.git(t, ctx, "init", "--bare", "-q", remote)
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	if err := CreateKeyFile(keyPath, source.repo.Root); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(source.w.Common, "gitlocal")
	for _, dir := range []string{"", "transactions", filepath.Join("workspaces", source.w.WorkspaceID)} {
		if err := os.MkdirAll(filepath.Join(state, dir), 0700); err != nil {
			t.Fatal(err)
		}
		name := ".gitlocal-tmp-0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a6" + string(rune('0'+len(dir)%10))
		if err := os.WriteFile(filepath.Join(state, dir, name), []byte("interrupted write\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	leftover := filepath.Join(state, "history-encrypt-0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69.git")
	if err := os.Mkdir(leftover, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := push(ctx, source.g, source.repo.Root, "backup", keyPath, func(context.Context, *gitexec.Runner, string, string, string, string) error {
		t.Fatal("an archive with unknown private state reached the transport")
		return nil
	})
	if !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), filepath.Base(leftover)) {
		t.Fatalf("push with an unknown private entry: %v", err)
	}
	if err := os.Remove(leftover); err != nil {
		t.Fatal(err)
	}
	ref := source.push(t, keyPath)
	stage, err := source.fetch(t, keyPath, ref, ref)
	if err != nil {
		t.Fatalf("fetch the backup: %v", err)
	}
	defer os.RemoveAll(stage)
	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(source.repo.GitPath, "clone", "-q", "--no-local", source.repo.Root, clone)
	cmd.Env = source.repo.Env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	info, err := AdoptStaged(ctx, source.g, clone, stage)
	if err != nil || info.RepositoryID != source.w.RepositoryID {
		t.Fatalf("restore a backup taken with leftover temporary files: %+v %v", info, err)
	}
	if data, err := history.Show(ctx, source.g, clone, source.w.WorkspaceID, "latest", "notes.local"); err != nil || string(data) != "saved\n" {
		t.Fatalf("restored history: %q %v", data, err)
	}
}
