package hook_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestInstallRefusesTrackedHooksDirectoryWithoutChanges(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	original := []byte("#!/bin/sh\necho team hook\n")
	if err := os.Mkdir(filepath.Join(repo.Root, ".githooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, ".githooks", "pre-commit"), original, 0700); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", ".githooks/pre-commit")
	repo.Git("commit", "-qm", "team hooks")
	repo.Git("config", "core.hooksPath", ".githooks")
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
	for _, args := range [][]string{{"hook", "install"}, {"hook", "install", "--allow-shared-hooks-dir"}, {"auto", "install"}} {
		out, err := runLocal(args...)
		if err == nil || !strings.Contains(out, "inside the worktree") || !strings.Contains(out, "guard staged") {
			t.Fatalf("%v into a tracked hooks directory: %v\n%s", args, err, out)
		}
	}
	if got, err := os.ReadFile(filepath.Join(repo.Root, ".githooks", "pre-commit")); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("refused install changed the tracked hook: %v %q", err, got)
	}
	if entries, err := os.ReadDir(filepath.Join(repo.Root, ".githooks")); err != nil || len(entries) != 1 {
		t.Fatalf("refused install added hook files: %v %v", err, entries)
	}
	if status := repo.Git("status", "--porcelain"); len(status) != 0 {
		t.Fatalf("refused install changed the worktree:\n%s", status)
	}
	if out, err := runLocal("hook", "status"); err != nil || !strings.Contains(out, "inside the worktree") || !strings.Contains(out, "tracked: pre-commit") {
		t.Fatalf("hook status did not explain the tracked hooks directory: %v\n%s", err, out)
	}
}

// A linked worktree inside the common Git directory is still a worktree: its
// tracked hooks directory must be refused, not treated as repository-private.
func TestInstallRefusesTrackedHooksInWorktreeInsideGitDirectory(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	original := []byte("#!/bin/sh\necho team hook\n")
	if err := os.Mkdir(filepath.Join(repo.Root, ".githooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, ".githooks", "pre-commit"), original, 0700); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", ".githooks/pre-commit")
	repo.Git("commit", "-qm", "team hooks")
	nested := filepath.Join(repo.Root, ".git", "wt2")
	repo.Git("worktree", "add", "-q", nested)
	repo.Git("-C", nested, "config", "core.hooksPath", filepath.Join(nested, ".githooks"))
	binary := testBinary(t)
	runLocal := func(args ...string) (string, error) {
		c := exec.Command(binary, args...)
		c.Dir, c.Env = nested, repo.RunnerEnv()
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := runLocal("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"hook", "install"}, {"auto", "install"}} {
		if out, err := runLocal(args...); err == nil {
			t.Fatalf("%v installed into a tracked hooks directory of a nested worktree:\n%s", args, out)
		}
	}
	if got, err := os.ReadFile(filepath.Join(nested, ".githooks", "pre-commit")); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("refused install changed the tracked hook: %v %q", err, got)
	}
	if status := repo.Git("-C", nested, "status", "--porcelain"); len(status) != 0 {
		t.Fatalf("refused install changed the nested worktree:\n%s", status)
	}
}
