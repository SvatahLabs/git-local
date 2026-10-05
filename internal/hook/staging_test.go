package hook_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestLeftoverStageFileIsIgnoredAndGuarded(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	binary := testBinary(t)
	runLocal := func(args ...string) (string, error) {
		c := exec.Command(binary, args...)
		c.Dir, c.Env = repo.Root, repo.RunnerEnv()
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := runLocal("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if err := os.Mkdir(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes", "secret"), []byte("local only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocal("track", "notes/secret"); err != nil {
		t.Fatalf("track: %v\n%s", err, out)
	}
	stage := "notes/.gitlocal-tmp-0b7a8c8e-4a7b-4c3e-9a1d-2f9e6b1c3d4e"
	if err := os.WriteFile(filepath.Join(repo.Root, filepath.FromSlash(stage)), []byte("decrypted partial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if status := string(repo.Git("status", "--porcelain", "--untracked-files=all")); strings.Contains(status, ".gitlocal-tmp-") {
		t.Fatalf("stage file is not ignored:\n%s", status)
	}
	repo.Git("add", "-A")
	if staged := string(repo.Git("diff", "--cached", "--name-only")); strings.Contains(staged, ".gitlocal-tmp-") {
		t.Fatalf("git add -A staged the stage file:\n%s", staged)
	}
	if out, err := runLocal("doctor"); !strings.Contains(out, stage) {
		t.Fatalf("doctor did not list the leftover stage file: %v\n%s", err, out)
	}

	if out, err := runLocal("hook", "install"); err != nil {
		t.Fatalf("hook install: %v\n%s", err, out)
	}
	repo.Git("add", "-f", "--", stage)
	commit := exec.Command(repo.GitPath, "commit", "-qm", "forced stage")
	commit.Dir, commit.Env = repo.Root, repo.Env
	if out, err := commit.CombinedOutput(); err == nil || !strings.Contains(string(out), stage) {
		t.Fatalf("commit of a forced stage file was not rejected: %v\n%s", err, out)
	}
	if out, err := runLocal("guard", "staged"); err == nil || !strings.Contains(out, "staging file") {
		t.Fatalf("guard staged accepted the stage file: %v\n%s", err, out)
	}
	repo.Git("commit", "--no-verify", "-qm", "forced stage")
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	push := exec.Command(repo.GitPath, "push", "origin", "main")
	push.Dir, push.Env = repo.Root, repo.Env
	if out, err := push.CombinedOutput(); err == nil || !strings.Contains(string(out), stage) {
		t.Fatalf("push of a stage file was not rejected: %v\n%s", err, out)
	}
}
