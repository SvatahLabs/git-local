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

func TestInstalledGuardBlocksForcedAddAndChainsExistingHook(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	repo.Env = append(repo.Env, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+home)
	binary := testBinary(t)
	runLocal := func(args ...string) (string, error) {
		c := exec.Command(binary, args...)
		c.Dir = repo.Root
		c.Env = repo.RunnerEnv()
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
	hooks := filepath.Join(repo.Root, "hooks-custom")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	repo.Git("config", "core.hooksPath", hooks)
	original := []byte("#!/bin/sh\nprintf 'old hook\\n' >> hook.log\n")
	originalPush := []byte("#!/bin/sh\nwhile read local_ref local_sha remote_ref remote_sha; do printf '%s\\n' \"$local_ref\" >> push-hook.log; done\n")
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), originalPush, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocal("hook", "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	installed, err := os.ReadFile(filepath.Join(hooks, "pre-commit"))
	if err != nil || !bytes.Contains(installed, original[10:]) {
		t.Fatalf("original hook not preserved in managed script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.txt"), []byte("normal update\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.txt")
	repo.Git("commit", "-qm", "normal")
	log, err := os.ReadFile(filepath.Join(repo.Root, "hook.log"))
	if err != nil || string(log) != "old hook\n" {
		t.Fatalf("existing hook did not run: %v %q", err, log)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	repo.Git("push", "origin", "main")
	pushLog, err := os.ReadFile(filepath.Join(repo.Root, "push-hook.log"))
	if err != nil || string(pushLog) != "refs/heads/main\n" {
		t.Fatalf("existing pre-push hook did not receive ref updates: %v %q", err, pushLog)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "other", linked)
	if err := os.MkdirAll(filepath.Join(linked, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "notes", "secret"), []byte("other checkout\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.At(linked, "add", "-f", "notes/secret")
	linkedCommit := exec.Command(repo.GitPath, "commit", "-qm", "cross-worktree")
	linkedCommit.Dir, linkedCommit.Env = linked, repo.Env
	linkedOut, linkedErr := linkedCommit.CombinedOutput()
	if linkedErr == nil || !strings.Contains(string(linkedOut), "notes/secret") {
		t.Fatalf("linked worktree leak was not blocked: %v\n%s", linkedErr, linkedOut)
	}
	repo.At(linked, "reset", "--hard", "-q")
	linkedLocal := exec.Command(binary, "init")
	linkedLocal.Dir, linkedLocal.Env = linked, repo.RunnerEnv()
	linkedInit, linkedInitErr := linkedLocal.CombinedOutput()
	if linkedInitErr != nil {
		t.Fatalf("linked init: %v\n%s", linkedInitErr, linkedInit)
	}
	linkedFields := strings.Fields(string(linkedInit))
	if len(linkedFields) < 4 {
		t.Fatalf("unexpected linked init output: %s", linkedInit)
	}
	retiredID := linkedFields[3]
	if err := os.WriteFile(filepath.Join(linked, "retired.txt"), []byte("retained selection\n"), 0600); err != nil {
		t.Fatal(err)
	}
	trackRetired := exec.Command(binary, "track", "retired.txt")
	trackRetired.Dir, trackRetired.Env = linked, repo.RunnerEnv()
	if output, err := trackRetired.CombinedOutput(); err != nil {
		t.Fatalf("linked track: %v\n%s", err, output)
	}
	repo.Git("worktree", "remove", "--force", linked)
	if out, err := runLocal("workspace", "retire", retiredID); err != nil {
		t.Fatalf("retire: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "retired.txt"), []byte("mainline leak\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "-f", "retired.txt")
	retiredCommit := exec.Command(repo.GitPath, "commit", "-qm", "retired-leak")
	retiredCommit.Dir, retiredCommit.Env = repo.Root, repo.Env
	retiredOut, retiredErr := retiredCommit.CombinedOutput()
	if retiredErr == nil || !strings.Contains(string(retiredOut), "retired.txt") || !strings.Contains(string(retiredOut), retiredID) {
		t.Fatalf("retired selection was not protected: %v\n%s", retiredErr, retiredOut)
	}
	repo.Git("reset", "-q", "--", "retired.txt")
	repo.Git("add", "-f", "notes/secret")
	c := exec.Command(repo.GitPath, "commit", "-qm", "forced")
	c.Dir = repo.Root
	c.Env = repo.Env
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "notes/secret") || !strings.Contains(string(out), "selected for local history") {
		t.Fatalf("forced add was not blocked: %v\n%s", err, out)
	}
	if guardOut, guardErr := runLocal("guard", "staged"); guardErr == nil || !strings.Contains(guardOut, "notes/secret") {
		t.Fatalf("direct staged guard missed selected path: %v\n%s", guardErr, guardOut)
	} else if exit, ok := guardErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("selected path exit code: %v", guardErr)
	}
	log, err = os.ReadFile(filepath.Join(repo.Root, "hook.log"))
	if err != nil || string(log) != "old hook\n" {
		t.Fatalf("old hook ran before guard rejection: %v %q", err, log)
	}
	repo.Git("commit", "--no-verify", "-qm", "leak")
	if err := os.Remove(filepath.Join(repo.Root, "notes", "secret")); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "-u", "notes/secret")
	if out, err := runLocal("guard", "staged"); err != nil {
		t.Fatalf("staged deletion should be allowed: %v\n%s", err, out)
	}
	repo.Git("commit", "-qm", "remove leak")
	push := exec.Command(repo.GitPath, "push", "origin", "main")
	push.Dir, push.Env = repo.Root, repo.Env
	pushOut, pushErr := push.CombinedOutput()
	if pushErr == nil || !strings.Contains(string(pushOut), "notes/secret") || !strings.Contains(string(pushOut), "commit ") {
		t.Fatalf("pre-push should reject no-verify leak: %v\n%s", pushErr, pushOut)
	}
	repo.Git("push", "--no-verify", "-u", "origin", "main")
	repo.Git("checkout", "-q", "-b", "clean-branch")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.txt"), []byte("clean branch update\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.txt")
	repo.Git("commit", "-qm", "clean branch update")
	newBranchPush := exec.Command(repo.GitPath, "push", "origin", "clean-branch")
	newBranchPush.Dir, newBranchPush.Env = repo.Root, repo.Env
	if out, err := newBranchPush.CombinedOutput(); err != nil {
		t.Fatalf("new branch should exclude commits already on the remote: %v\n%s", err, out)
	}
	repo.Git("checkout", "-q", "main")
	if out, err := runLocal("doctor"); err != nil || !strings.Contains(out, "upstream history") || !strings.Contains(out, "notes/secret") {
		t.Fatalf("doctor missed leaked upstream history: %v\n%s", err, out)
	}
	repo.Git("config", "core.hooksPath", "hooks-moved")
	if out, err := runLocal("hook", "status"); err != nil || !strings.Contains(out, "previous installation") {
		t.Fatalf("moved hooks path not reported: %v\n%s", err, out)
	}
	if out, err := runLocal("doctor"); err != nil || !strings.Contains(out, "core.hooksPath changed") {
		t.Fatalf("doctor missed moved hooks path: %v\n%s", err, out)
	}
	repo.Git("config", "core.hooksPath", hooks)
	if out, err := runLocal("hook", "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	restored, err := os.ReadFile(filepath.Join(hooks, "pre-commit"))
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("uninstall did not restore original: %v", err)
	}
	restoredPush, err := os.ReadFile(filepath.Join(hooks, "pre-push"))
	if err != nil || !bytes.Equal(restoredPush, originalPush) {
		t.Fatalf("uninstall did not restore original pre-push: %v", err)
	}
	legacy := []byte("#!/bin/sh\n# git-local managed pre-commit hook\nexit 0\n")
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit.git-local-original"), original, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocal("hook", "install", "--pre-commit"); err != nil {
		t.Fatalf("legacy migration install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(hooks, "pre-commit.git-local-original")); !os.IsNotExist(err) {
		t.Fatalf("legacy backup not consumed: %v", err)
	}
	if out, err := runLocal("hook", "uninstall", "--pre-commit"); err != nil {
		t.Fatalf("legacy migration uninstall: %v\n%s", err, out)
	}
	restored, err = os.ReadFile(filepath.Join(hooks, "pre-commit"))
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("legacy migration did not restore original: %v", err)
	}
}
