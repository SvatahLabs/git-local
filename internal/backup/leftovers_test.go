package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/storage"
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

// A seed's file list is state only while its completed journal exists: push
// leaves out a list an interrupted seed or abandoned migration orphaned, and
// anything else there (a copy of a list, a directory), and restore refuses a
// backup that carries one.
func TestPushSkipsOrphanSeedFileListsAndRestoreRefusesThem(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	source := newBoundSource(t, remote, "saved\n")
	ctx := context.Background()
	source.git(t, ctx, "init", "--bare", "-q", remote)
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	if err := CreateKeyFile(keyPath, source.repo.Root); err != nil {
		t.Fatal(err)
	}
	const orphan = "0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69.json"
	lists := filepath.Join(source.w.Common, "gitlocal", "transactions", "seed-files")
	if err := os.MkdirAll(lists, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{orphan, orphan + ".bak", "copy/" + orphan} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(lists, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lists, name), []byte("{\"files\": []}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ref := source.push(t, keyPath)
	stage, err := source.fetch(t, keyPath, ref, ref)
	if err != nil {
		t.Fatalf("fetch the backup: %v", err)
	}
	defer os.RemoveAll(stage)
	if entries, err := os.ReadDir(filepath.Join(stage, "state", "transactions", "seed-files")); err == nil && len(entries) > 0 || err != nil && !os.IsNotExist(err) {
		t.Fatalf("push archived seed-files entries no completed seed names: %v %v", entries, err)
	}
	if _, err := ValidateStaged(ctx, source.g, stage); err != nil {
		t.Fatalf("restore of the backup: %v", err)
	}
	staged := filepath.Join(stage, "state", "transactions", "seed-files", orphan)
	if err := os.MkdirAll(filepath.Dir(staged), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("{\"files\": []}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateStaged(ctx, source.g, stage); err == nil || !strings.Contains(err.Error(), "no completed seed journal") {
		t.Fatalf("restore of a backup with an orphan seed file list: %v", err)
	}
}

// GL-119: a completed seed journal whose file list is lost would make the
// backup unrestorable (restore reads the journal with its list); push refuses
// it, naming the list and the way out.
func TestPushRefusesCompletedSeedWithoutItsFileList(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	source := newBoundSource(t, remote, "saved\n")
	ctx := context.Background()
	source.git(t, ctx, "init", "--bare", "-q", remote)
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	if err := CreateKeyFile(keyPath, source.repo.Root); err != nil {
		t.Fatal(err)
	}
	const id = "0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69"
	completed := filepath.Join(source.w.Common, "gitlocal", "transactions", "completed")
	if err := os.MkdirAll(completed, 0700); err != nil {
		t.Fatal(err)
	}
	journal := `{"schemaVersion": 1, "kind": "seed", "id": "` + id + `", "files": [], "filesDigest": "` + strings.Repeat("a", 64) + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(completed, "seed-"+id+".json"), []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := push(ctx, source.g, source.repo.Root, "backup", keyPath, func(context.Context, *gitexec.Runner, string, string, string, string) error {
		t.Fatal("an archive with a seed journal missing its file list reached the transport")
		return nil
	})
	if !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "seed-files/"+id+".json") || !strings.Contains(err.Error(), "recover "+id+" --abandon") {
		t.Fatalf("push with a completed seed missing its file list: %v", err)
	}
}
