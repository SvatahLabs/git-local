// Package testutil creates disposable, configuration-isolated real Git fixtures.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type Repo struct {
	T       testing.TB
	Root    string
	Env     []string
	GitPath string
}

// RefFormat is the ref backend New creates repositories with: empty for Git's
// default, or "reftable". A package's TestMain sets it between runs to run
// its tests again on the other backend (GL-132).
var RefFormat string

// ReftableSupported reports whether Git can create a reftable repository.
func ReftableSupported() bool {
	dir, err := os.MkdirTemp("", "gitlocal-reftable-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command("git", "init", "-q", "--template=", "--ref-format=reftable", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	return cmd.Run() == nil
}

// MainWithReftable is a package's TestMain: it runs the tests, then, unless
// they failed, once is set (a child process that runs one command) or Git
// can't create a reftable repository, runs them again with every New
// repository on the reftable ref backend, Git 3.0's default (GL-132).
func MainWithReftable(m *testing.M, once bool) int {
	code := m.Run()
	if code != 0 || once || !ReftableSupported() {
		return code
	}
	fmt.Fprintln(os.Stderr, "=== the same tests on the reftable ref backend (GL-132)")
	RefFormat = "reftable"
	return m.Run()
}

func New(t testing.TB) *Repo {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(k), "GIT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@localhost", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@localhost", "GIT_TERMINAL_PROMPT=0")
	r := &Repo{t, root, env, executable}
	args := []string{"init", "-q", "-b", "main", "--template="}
	if RefFormat != "" {
		args = append(args, "--ref-format="+RefFormat)
	}
	r.Git(args...)
	return r
}

// NewReftable is New with Git's reftable ref backend: refs/heads is then a
// file, and each worktree keeps its own refs, HEAD included, in a reftable
// stack.
func NewReftable(t testing.TB) *Repo {
	t.Helper()
	r := New(t)
	if err := os.RemoveAll(filepath.Join(r.Root, ".git")); err != nil {
		t.Fatal(err)
	}
	r.Git("init", "-q", "-b", "main", "--template=", "--ref-format=reftable")
	return r
}

func (r *Repo) Git(args ...string) []byte { return r.At(r.Root, args...) }
func (r *Repo) At(cwd string, args ...string) []byte {
	r.T.Helper()
	cmd := exec.Command(r.GitPath, args...)
	cmd.Dir = cwd
	cmd.Env = r.Env
	b, err := cmd.CombinedOutput()
	if err != nil {
		r.T.Fatalf("fixture git %v: %v\n%s", args, err, b)
	}
	return b
}

// RunnerEnv keeps user configuration isolated through HOME, avoiding the
// inherited GIT_CONFIG_* contexts deliberately rejected by production runners.
func (r *Repo) RunnerEnv() []string {
	r.T.Helper()
	home := filepath.Join(r.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		r.T.Fatal(err)
	}
	var env []string
	for _, entry := range r.Env {
		k, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(k), "GIT_") && k != "HOME" && k != "XDG_CONFIG_HOME" && k != "USERPROFILE" {
			env = append(env, entry)
		}
	}
	return append(env, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+home)
}
func (r *Repo) Commit() {
	r.T.Helper()
	if err := os.WriteFile(filepath.Join(r.Root, "app.txt"), []byte("application\n"), 0600); err != nil {
		r.T.Fatal(err)
	}
	r.Git("add", "app.txt")
	r.Git("commit", "-qm", "initial")
}
