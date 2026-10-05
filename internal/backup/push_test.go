package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/overlay"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestPushUsesIsolatedEncryptedCommitAndRefusesOriginHost(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	g, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	w, err := workspace.Init(context.Background(), g, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("SECRET_BACKUP_CONTENT_99115")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), secret, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(context.Background(), g, repo.Root, true, "notes.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(context.Background(), g, repo.Root, "save secret"); err != nil {
		t.Fatal(err)
	}
	repo.Git("remote", "add", "origin", "git@origin.example:project/main.git")
	repo.Git("remote", "add", "backup", "ssh://git@backup.example/project/archive.git")
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	if err := CreateKeyFile(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	key, err := LoadKeyFile(keyPath, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	init := exec.Command(repo.GitPath, "init", "--bare", "-q", remoteDir)
	init.Env = repo.Env
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("init test remote: %v %s", err, out)
	}
	var encrypted []byte
	transport := func(ctx context.Context, r *gitexec.Runner, store, url, commit, ref string) error {
		if url != "ssh://git@backup.example/project/archive.git" {
			t.Fatalf("unexpected validated URL: %s", url)
		}
		listing, err := r.History(ctx, store, nil, "ls-tree", "-r", commit)
		if err != nil {
			return err
		}
		fields := strings.Fields(string(listing))
		if len(fields) < 4 || fields[3] != "archive.enc" {
			t.Fatalf("isolated commit tree: %s", listing)
		}
		encrypted, err = r.History(ctx, store, nil, "cat-file", "blob", fields[2])
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, repo.GitPath, "-c", "protocol.file.allow=always", "--git-dir="+store, "push", "file://"+remoteDir, commit+":"+ref)
		cmd.Env = repo.RunnerEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("push to disposable test remote: %v %s", err, out)
		}
		return nil
	}
	result, err := push(context.Background(), g, repo.Root, "backup", keyPath, transport)
	if err != nil || result.Destinations != 1 || !strings.HasPrefix(result.Ref, "refs/gitlocal/backups/") {
		t.Fatal(result, err)
	}
	if bytes.Contains(encrypted, secret) {
		t.Fatal("remote archive contained plaintext")
	}
	var plain bytes.Buffer
	if err := Decrypt(&plain, bytes.NewReader(encrypted), key); err != nil {
		t.Fatal(err)
	}
	foundHistory := false
	treader := tar.NewReader(&plain)
	for {
		header, err := treader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(header.Name, "state/history.git/objects/") {
			foundHistory = true
		}
	}
	if !foundHistory {
		t.Fatal("encrypted archive omitted private history")
	}
	mainHead := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	probe := exec.Command(repo.GitPath, "--git-dir="+remoteDir, "cat-file", "-e", mainHead)
	probe.Env = repo.Env
	if err := probe.Run(); err == nil {
		t.Fatal("mainline Git object was pushed to backup remote")
	}
	refs := exec.Command(repo.GitPath, "--git-dir="+remoteDir, "show-ref", result.Ref)
	refs.Env = repo.Env
	if err := refs.Run(); err != nil {
		t.Fatal("encrypted backup ref missing from remote", err)
	}
	stage, err := fetchStaged(context.Background(), g, repo.Root, "backup", result.Ref, keyPath, func(ctx context.Context, _ *gitexec.Runner, store, url, ref string) error {
		if url != "ssh://git@backup.example/project/archive.git" {
			t.Fatalf("unexpected restore URL: %s", url)
		}
		cmd := exec.CommandContext(ctx, repo.GitPath, "-c", "protocol.file.allow=always", "--git-dir="+store, "fetch", "--no-tags", "file://"+remoteDir, ref+":refs/heads/import")
		cmd.Env = repo.RunnerEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fetch from disposable test remote: %v %s", err, out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	if _, err := os.Stat(filepath.Join(stage, "state", "history.git", "config")); err != nil {
		t.Fatal("fetched backup was not extracted", err)
	}
	clone := filepath.Join(t.TempDir(), "fresh-clone")
	cloneCommand := exec.Command(repo.GitPath, "clone", "-q", "--no-local", repo.Root, clone)
	cloneCommand.Env = repo.Env
	if out, err := cloneCommand.CombinedOutput(); err != nil {
		t.Fatalf("clone fresh checkout: %v %s", err, out)
	}
	installed, err := AdoptStaged(context.Background(), g, clone, stage)
	if err != nil || installed.RepositoryID != w.RepositoryID {
		t.Fatalf("adopt fetched backup: %+v %v", installed, err)
	}
	log, err := history.Log(context.Background(), g, clone, 10)
	if err != nil || len(log) != 1 {
		t.Fatalf("fetched backup history in fresh clone: %+v %v", log, err)
	}
	repo.Git("remote", "set-url", "backup", "git@origin.example:project/archive.git")
	called := false
	_, err = push(context.Background(), g, repo.Root, "backup", keyPath, func(context.Context, *gitexec.Runner, string, string, string, string) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatal("same-host backup destination reached transport", err)
	}
	base := []byte("port = 80\n")
	local := []byte("port = 8080\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.txt"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.txt")
	repo.Git("commit", "-qm", "add app")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.txt"), local, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := overlay.Add(context.Background(), g, repo.Root, "sample", "app.txt", executable); err != nil {
		t.Fatal(err)
	}
	repo.Git("remote", "set-url", "backup", "ssh://git@backup.example/project/archive.git")
	withOverlay, err := push(context.Background(), g, repo.Root, "backup", keyPath, transport)
	if err != nil || withOverlay.Destinations != 1 {
		t.Fatalf("active overlay backup failed: %+v %v", withOverlay, err)
	}
	if bytes.Contains(encrypted, local) {
		t.Fatal("remote overlay archive contained plaintext patch")
	}
	overlayStage, err := fetchStaged(context.Background(), g, repo.Root, "backup", withOverlay.Ref, keyPath, func(ctx context.Context, _ *gitexec.Runner, store, _, ref string) error {
		cmd := exec.CommandContext(ctx, repo.GitPath, "-c", "protocol.file.allow=always", "--git-dir="+store, "fetch", "--no-tags", "file://"+remoteDir, ref+":refs/heads/import")
		cmd.Env = repo.RunnerEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fetch overlay backup: %v %s", err, out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(overlayStage)
	overlayClone := filepath.Join(t.TempDir(), "overlay-clone")
	cloneCommand = exec.Command(repo.GitPath, "clone", "-q", "--no-local", repo.Root, overlayClone)
	cloneCommand.Env = repo.Env
	if out, err := cloneCommand.CombinedOutput(); err != nil {
		t.Fatalf("clone for overlay adoption: %v %s", err, out)
	}
	overlayClone, err = filepath.EvalSymlinks(overlayClone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adoptStaged(context.Background(), g, overlayClone, overlayStage, func(step string) error {
		if step == "journal" {
			return errors.New("simulated interruption")
		}
		return nil
	}); err == nil {
		t.Fatal("overlay adoption interruption was ignored")
	}
	recovered, err := RecoverRestore(context.Background(), g, overlayClone)
	if err != nil || recovered.ArchivedOverlays != 1 {
		t.Fatalf("recover backup with saved overlay: %v", err)
	}
	version, err := overlay.LoadVersion(context.Background(), filepath.Join(overlayClone, ".git"), w.WorkspaceID, "sample", 0)
	if err != nil || !bytes.Equal(version.Local, local) || version.Manifest.State != "removed" {
		t.Fatalf("adopted overlay version: %+v %v", version.Manifest, err)
	}
	if active, err := overlay.HasAny(context.Background(), filepath.Join(overlayClone, ".git")); err != nil || active {
		t.Fatalf("adopted overlay remained active: %v %v", active, err)
	}
	config := exec.Command(repo.GitPath, "config", "--get", "filter.gitlocal-overlay.process")
	config.Dir, config.Env = overlayClone, repo.Env
	if out, err := config.CombinedOutput(); err == nil || len(out) != 0 {
		t.Fatalf("source filter was installed in clone: %v %q", err, out)
	}
	if err := os.WriteFile(filepath.Join(overlayClone, "app.txt"), version.Local, 0600); err != nil {
		t.Fatal(err)
	}
	reactivated, err := overlay.Add(context.Background(), g, overlayClone, "sample", "app.txt", executable)
	if err != nil || reactivated.Version != 2 || reactivated.State == "removed" {
		t.Fatalf("reactivate saved overlay: %+v %v", reactivated, err)
	}
	corrupt := filepath.Join(w.Common, "gitlocal", "overlays", w.WorkspaceID, "sample", "v000001", "local")
	if err := os.WriteFile(corrupt, []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	called = false
	_, err = push(context.Background(), g, repo.Root, "backup", keyPath, func(context.Context, *gitexec.Runner, string, string, string, string) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("corrupt overlay reached backup transport: %v, called %v", err, called)
	}
}
