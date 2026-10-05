package hook_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestGuardsAllowUnselectablePathsAndCatchTheirAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("these names are not representable on Windows")
	}
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
	if out, err := runLocal("hook", "install"); err != nil {
		t.Fatalf("hook install: %v\n%s", err, out)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	repo.Git("push", "-q", "origin", "main")

	names := []string{"aux.c", "con.h", "nul.txt", `a\b.md`, "name.", "trailing ", "\xffbyte.txt"}
	added := 0
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(repo.Root, name), []byte("ordinary\n"), 0600); err != nil {
			t.Logf("filesystem refused %q: %v", name, err)
			continue
		}
		repo.Git("add", "--", name)
		added++
	}
	if added < 6 {
		t.Fatalf("only %d test names could be created", added)
	}
	commit := exec.Command(repo.GitPath, "commit", "-qm", "unselectable names")
	commit.Dir, commit.Env = repo.Root, repo.Env
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit of unselected names was refused: %v\n%s", err, out)
	}
	push := exec.Command(repo.GitPath, "push", "-q", "origin", "main")
	push.Dir, push.Env = repo.Root, repo.Env
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push of unselected names was refused: %v\n%s", err, out)
	}
	if out, err := runLocal("doctor"); err != nil || strings.Contains(out, "ERROR upstream history") {
		t.Fatalf("doctor history audit failed on unselectable names: %v\n%s", err, out)
	}

	for _, alias := range []string{`notes\secret`, "notes/secret.", "NOTES/SECRET "} {
		if err := os.WriteFile(filepath.Join(repo.Root, alias), []byte("alias\n"), 0600); err != nil {
			t.Logf("filesystem refused %q: %v", alias, err)
			continue
		}
		repo.Git("add", "-f", "--", alias)
		guard, err := runLocal("guard", "staged")
		if err == nil || !strings.Contains(guard, "selected for local history") {
			t.Fatalf("guard accepted %q, an alias of a selected path: %v\n%s", alias, err, guard)
		}
		repo.Git("rm", "-q", "--cached", "--", alias)
		if err := os.Remove(filepath.Join(repo.Root, alias)); err != nil {
			t.Fatal(err)
		}
	}
}

// An overlay patch staged under an alias spelling of the overlay's path (here a
// trailing dot, a separate file on POSIX) must still be caught by the guard.
func TestGuardCatchesOverlayPatchUnderAliasSpelling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a trailing-dot name is not representable on Windows")
	}
	repo := testutil.New(t)
	repo.Git("config", "core.autocrlf", "false")
	base := "port = 80\na\nb\nc\nd\ne\nf\ng\n"
	local := strings.Replace(base, "port = 80", "port = 8080", 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "config")
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
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte(local), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocal("overlay", "add", "port", "--", "app.conf"); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	}
	if out, err := runLocal("hook", "install"); err != nil {
		t.Fatalf("hook install: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf."), []byte(local), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "--", "app.conf.")
	commit := exec.Command(repo.GitPath, "commit", "-qm", "alias leak")
	commit.Dir, commit.Env = repo.Root, repo.Env
	if out, err := commit.CombinedOutput(); err == nil || !strings.Contains(string(out), "app.conf.") {
		t.Fatalf("guard did not catch the overlay patch under an alias spelling: %v\n%s", err, out)
	}
}
