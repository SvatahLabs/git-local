package hook_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// hookRepo is an initialized repository with a function that runs a built
// git-local in it.
func hookRepo(t *testing.T, binary string, env func(*testutil.Repo) []string) (*testutil.Repo, func(args ...string) (string, error)) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	if env == nil {
		env = func(r *testutil.Repo) []string { return r.RunnerEnv() }
	}
	run := func(args ...string) (string, error) {
		c := exec.Command(binary, args...)
		c.Dir, c.Env = repo.Root, env(repo)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return repo, run
}

func mustRun(t *testing.T, run func(args ...string) (string, error), args ...string) string {
	t.Helper()
	out, err := run(args...)
	if err != nil {
		t.Fatalf("git local %v: %v\n%s", args, err, out)
	}
	return out
}

func readHook(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hasBlock(data []byte, name string) bool {
	return bytes.Contains(data, []byte("# >>> git-local "+name+" >>>")) && bytes.Contains(data, []byte("# <<< git-local "+name+" <<<"))
}

// TestHookInstallAndUninstallRecoverFromStaleRecords covers the ways a hook
// file and git-local's record of it can disagree: the file moved away, its
// block removed, or a crash between the record and the file writes.
func TestHookInstallAndUninstallRecoverFromStaleRecords(t *testing.T) {
	binary := testBinary(t)
	repo, run := hookRepo(t, binary, nil)
	hooks := filepath.Join(repo.Root, ".git", "hooks")
	preCommit := filepath.Join(hooks, "pre-commit")
	manifest := filepath.Join(repo.Root, ".git", "gitlocal", "hook-install.json")
	original := []byte("#!/bin/sh\necho own hook\n")
	if err := os.MkdirAll(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preCommit, original, 0700); err != nil {
		t.Fatal(err)
	}
	uninstalled := func(step string) {
		t.Helper()
		if _, err := os.Stat(manifest); !os.IsNotExist(err) {
			t.Fatalf("%s: hook records remain after uninstall: %v", step, err)
		}
		if _, err := os.Stat(filepath.Join(hooks, "pre-push")); !os.IsNotExist(err) {
			t.Fatalf("%s: created pre-push hook remains: %v", step, err)
		}
	}

	// The pre-commit framework moves the hook aside, block included.
	mustRun(t, run, "hook", "install")
	moved := readHook(t, preCommit)
	if err := os.Rename(preCommit, preCommit+".legacy"); err != nil {
		t.Fatal(err)
	}
	mustRun(t, run, "hook", "uninstall")
	uninstalled("moved hook")
	if _, err := os.Stat(preCommit); !os.IsNotExist(err) {
		t.Fatalf("uninstall recreated the moved hook: %v", err)
	}
	if data := readHook(t, preCommit+".legacy"); !bytes.Equal(data, moved) {
		t.Fatalf("uninstall changed the moved hook: %q", data)
	}
	if err := os.WriteFile(preCommit, original, 0700); err != nil {
		t.Fatal(err)
	}
	// Moved away again, then reinstalled without uninstalling first.
	mustRun(t, run, "hook", "install")
	if err := os.Rename(preCommit, preCommit+".legacy"); err != nil {
		t.Fatal(err)
	}
	mustRun(t, run, "hook", "install")
	if !hasBlock(readHook(t, preCommit), "pre-commit") {
		t.Fatal("reinstall did not write the moved hook's block")
	}
	mustRun(t, run, "hook", "uninstall")
	uninstalled("reinstalled moved hook")
	if _, err := os.Stat(preCommit); !os.IsNotExist(err) {
		t.Fatalf("uninstall left the hook it created: %v", err)
	}
	if err := os.WriteFile(preCommit, original, 0700); err != nil {
		t.Fatal(err)
	}

	// The user removes the managed block by hand.
	mustRun(t, run, "hook", "install")
	edited := []byte("#!/bin/sh\necho edited hook\n")
	if err := os.WriteFile(preCommit, edited, 0700); err != nil {
		t.Fatal(err)
	}
	mustRun(t, run, "hook", "install")
	if data := readHook(t, preCommit); !hasBlock(data, "pre-commit") || !bytes.Contains(data, []byte("echo edited hook")) {
		t.Fatalf("reinstall over an edited hook: %q", data)
	}
	if err := os.WriteFile(preCommit, edited, 0700); err != nil {
		t.Fatal(err)
	}
	mustRun(t, run, "hook", "uninstall")
	uninstalled("removed block")
	if data := readHook(t, preCommit); !bytes.Equal(data, edited) {
		t.Fatalf("uninstall changed a hook without a block: %q", data)
	}
	if err := os.WriteFile(preCommit, original, 0700); err != nil {
		t.Fatal(err)
	}

	// A crash after writing the hooks but before recording them leaves
	// managed blocks with no record.
	mustRun(t, run, "hook", "install")
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	mustRun(t, run, "hook", "install")
	mustRun(t, run, "hook", "uninstall")
	uninstalled("unrecorded blocks, reinstalled")
	if data := readHook(t, preCommit); !bytes.Equal(data, original) {
		t.Fatalf("uninstall after adopting blocks: %q", data)
	}
	mustRun(t, run, "hook", "install")
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	mustRun(t, run, "hook", "uninstall")
	uninstalled("unrecorded blocks")
	if data := readHook(t, preCommit); !bytes.Equal(data, original) {
		t.Fatalf("uninstall left an unrecorded block: %q", data)
	}

	// A crash after recording the hooks but before writing them leaves
	// records with no block.
	mustRun(t, run, "hook", "install")
	if err := os.WriteFile(preCommit, original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(hooks, "pre-push")); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, run, "hook", "status"); !strings.Contains(out, "pre-commit: missing") {
		t.Fatalf("status with records but no blocks:\n%s", out)
	}
	mustRun(t, run, "hook", "install")
	if out := mustRun(t, run, "hook", "status"); !strings.Contains(out, "pre-commit: installed") || !strings.Contains(out, "pre-push: installed") {
		t.Fatalf("status after reinstall:\n%s", out)
	}
	mustRun(t, run, "hook", "uninstall")
	uninstalled("unwritten blocks")
	if data := readHook(t, preCommit); !bytes.Equal(data, original) {
		t.Fatalf("final uninstall: %q", data)
	}
}

// TestMovedHooksPathReinstalls moves core.hooksPath to a new directory, once
// empty and once holding a copy of the old hooks.
func TestMovedHooksPathReinstalls(t *testing.T) {
	binary := testBinary(t)
	repo, run := hookRepo(t, binary, nil)
	mustRun(t, run, "hook", "install")
	old := filepath.Join(repo.Root, ".git", "hooks")
	for i, copyOld := range []bool{false, true} {
		moved := filepath.Join(repo.Root, ".git", "hooks-moved"+string(rune('a'+i)))
		if err := os.Mkdir(moved, 0700); err != nil {
			t.Fatal(err)
		}
		if copyOld {
			for _, name := range []string{"pre-commit", "pre-push"} {
				if err := os.WriteFile(filepath.Join(moved, name), readHook(t, filepath.Join(old, name)), 0700); err != nil {
					t.Fatal(err)
				}
			}
		}
		repo.Git("config", "core.hooksPath", moved)
		mustRun(t, run, "hook", "install")
		if out := mustRun(t, run, "hook", "status"); !strings.Contains(out, "pre-commit: installed ("+moved+")") || !strings.Contains(out, "previous installation") {
			t.Fatalf("status after moving hooks (copy %t):\n%s", copyOld, out)
		}
	}
	mustRun(t, run, "hook", "uninstall")
	for _, dir := range []string{old, filepath.Join(repo.Root, ".git", "hooks-moveda"), filepath.Join(repo.Root, ".git", "hooks-movedb")} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("uninstall left hooks in %s: %v %v", dir, entries, err)
		}
	}
}
