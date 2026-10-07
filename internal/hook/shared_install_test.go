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

// TestSecondRepositoryUsesSharedHooksDirectory: repo A installs into a global
// hooks directory; repo B's install is a no-op that says so, B's status
// reports the guards installed, and A's uninstall warns that B loses them.
func TestSecondRepositoryUsesSharedHooksDirectory(t *testing.T) {
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	shared := filepath.Join(t.TempDir(), "shared-hooks")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[core]\n\thooksPath = "+filepath.ToSlash(shared)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := testBinary(t)
	env := func(r *testutil.Repo) []string { return withHome(r.RunnerEnv(), home) }
	newRepo := func() (*testutil.Repo, func(args ...string) (string, error)) {
		repo := testutil.New(t)
		repo.Env = withGlobalConfig(repo.Env, home, global)
		repo.Commit()
		run := func(args ...string) (string, error) {
			c := exec.Command(binary, args...)
			c.Dir, c.Env = repo.Root, env(repo)
			out, err := c.CombinedOutput()
			return string(out), err
		}
		mustRun(t, run, "init")
		return repo, run
	}
	_, runA := newRepo()
	mustRun(t, runA, "hook", "install", "--allow-shared-hooks-dir")
	before := readHook(t, filepath.Join(shared, "pre-commit"))

	b, runB := newRepo()
	if out := mustRun(t, runB, "hook", "install", "--allow-shared-hooks-dir"); !strings.Contains(out, "already installed by another repository") {
		t.Fatalf("B's install output:\n%s", out)
	}
	if after := readHook(t, filepath.Join(shared, "pre-commit")); !bytes.Equal(after, before) {
		t.Fatalf("B's install rewrote A's hook:\n%s", after)
	}
	out := mustRun(t, runB, "hook", "status")
	if !strings.Contains(out, "pre-commit: installed by another repository") || !strings.Contains(out, "pre-push: installed by another repository") || strings.Contains(out, "--allow-shared-hooks-dir") {
		t.Fatalf("B's status:\n%s", out)
	}
	if out := mustRun(t, runB, "doctor"); strings.Contains(out, "guard is not installed") {
		t.Fatalf("B's doctor:\n%s", out)
	}
	// The guard protects B.
	if err := os.WriteFile(filepath.Join(b.Root, "secret.txt"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, runB, "track", "secret.txt")
	b.Git("add", "-f", "secret.txt")
	commit := exec.Command(b.GitPath, "commit", "-qm", "leak")
	commit.Dir, commit.Env = b.Root, b.Env
	if out, err := commit.CombinedOutput(); err == nil || !strings.Contains(string(out), "secret.txt") {
		t.Fatalf("B's commit of a selected file was not refused: %v\n%s", err, out)
	}
	b.Git("reset", "-q", "--", "secret.txt")
	// B's uninstall leaves A's hooks alone.
	mustRun(t, runB, "hook", "uninstall")
	if after := readHook(t, filepath.Join(shared, "pre-commit")); !bytes.Equal(after, before) {
		t.Fatalf("B's uninstall changed A's hook:\n%s", after)
	}
	if out := mustRun(t, runA, "hook", "uninstall"); !strings.Contains(out, "other repositories") {
		t.Fatalf("A's uninstall from a shared directory did not warn:\n%s", out)
	}
	if out := mustRun(t, runB, "hook", "status"); !strings.Contains(out, "pre-commit: missing") || !strings.Contains(out, "--allow-shared-hooks-dir") {
		t.Fatalf("B's status after A's uninstall:\n%s", out)
	}
}
