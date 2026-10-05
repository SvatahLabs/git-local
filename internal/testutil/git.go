// Package testutil creates disposable, configuration-isolated real Git fixtures.
package testutil

import (
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
	r.Git("init", "-q", "-b", "main", "--template=")
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
