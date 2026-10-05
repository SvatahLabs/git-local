package hook_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// withHome points git-local at an isolated HOME whose .gitconfig is the global
// config; git-local refuses an inherited GIT_CONFIG_GLOBAL.
func withHome(env []string, home string) []string {
	out := []string{}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "GIT_CONFIG_GLOBAL" && key != "HOME" && key != "USERPROFILE" && key != "XDG_CONFIG_HOME" {
			out = append(out, entry)
		}
	}
	return append(out, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+home)
}

// withGlobalConfig points Git at the same isolated global config.
func withGlobalConfig(env []string, home, config string) []string {
	return append(withHome(env, home), "GIT_CONFIG_GLOBAL="+config)
}

func TestSharedHooksLeaveUninitializedRepositoriesWorking(t *testing.T) {
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	shared := filepath.Join(t.TempDir(), "shared-hooks")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[core]\n\thooksPath = "+filepath.ToSlash(shared)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "pre-commit"), []byte("#!/bin/sh\nprintf 'own hook\\n' >> \"$(git rev-parse --show-toplevel)/own-hook.log\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	binary := testBinary(t)

	a := testutil.New(t)
	a.Env = withGlobalConfig(a.Env, home, global)
	a.Commit()
	runA := func(args ...string) (string, error) {
		c := exec.Command(binary, args...)
		c.Dir, c.Env = a.Root, withHome(a.RunnerEnv(), home)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := runA("init"); err != nil {
		t.Fatalf("init A: %v\n%s", err, out)
	}
	if out, err := runA("hook", "install"); err != nil {
		t.Fatalf("hook install in A: %v\n%s", err, out)
	}

	b := testutil.New(t)
	b.Env = withGlobalConfig(b.Env, home, global)
	b.Commit()
	if log, err := os.ReadFile(filepath.Join(b.Root, "own-hook.log")); err != nil || string(log) != "own hook\n" {
		t.Fatalf("B's commit did not run its own hook after the managed block: %v %q", err, log)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	b.Git("init", "-q", "--bare", remote)
	b.Git("remote", "add", "origin", remote)
	b.Git("push", "-q", "origin", "main")

	// Damaged state still fails closed: the gitlocal directory exists but its
	// lock and identity files don't.
	if err := os.Mkdir(filepath.Join(b.Root, ".git", "gitlocal"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Root, "app.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b.Git("add", "app.txt")
	commit := exec.Command(b.GitPath, "commit", "-qm", "partial state")
	commit.Dir, commit.Env = b.Root, b.Env
	if out, err := commit.CombinedOutput(); err == nil {
		t.Fatalf("commit with partial git-local state was not refused:\n%s", out)
	}
}
