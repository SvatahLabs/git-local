// Package testutil creates disposable, configuration-isolated real Git fixtures.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// RefFormatArgs is the --ref-format option that gives a repository the ref
// backend of the current pass, for a fixture that makes one without New, such
// as git clone, which doesn't take the source's format.
func RefFormatArgs() []string {
	if RefFormat == "" {
		return nil
	}
	return []string{"--ref-format=" + RefFormat}
}

// isolatedEnv is the environment without GIT_* variables, such as a GIT_DIR
// inherited from a hook, and without system or global configuration.
func isolatedEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(k), "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

// ReftableSupported reports whether Git can create a reftable repository and,
// when it can't, Git's diagnostic.
func ReftableSupported() (bool, string) {
	dir, err := os.MkdirTemp("", "gitlocal-reftable-")
	if err != nil {
		return false, err.Error()
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command("git", "init", "-q", "--template=", "--ref-format=reftable", dir)
	cmd.Env = isolatedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, strings.TrimSpace(fmt.Sprintf("%v\n%s", err, out))
	}
	return true, ""
}

// gitHasReftable reports whether the installed Git's version has the reftable
// backend (Git 2.45 and later), so a failed probe is the environment's fault.
func gitHasReftable() bool {
	cmd := exec.Command("git", "version")
	cmd.Env = isolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(out), "git version %d.%d", &major, &minor); err != nil {
		return false
	}
	return major > 2 || major == 2 && minor >= 45
}

// MainWithReftable is a package's TestMain: it runs the tests, then, unless
// they failed or once is set (a child process that runs one command), runs
// them again with every New repository on the reftable ref backend, Git 3.0's
// default (GL-132). If Git can't create a reftable repository the second pass
// is skipped with a message, and the package fails when Git's version has
// reftable, as every supported Git does: the environment is then at fault.
func MainWithReftable(m *testing.M, once bool) int {
	code := m.Run()
	if code != 0 || once {
		return code
	}
	if ok, diagnostic := ReftableSupported(); !ok {
		if gitHasReftable() {
			fmt.Fprintf(os.Stderr, "=== FAIL: the reftable pass (GL-132) can't run: git init --ref-format=reftable failed:\n%s\n", diagnostic)
			return 1
		}
		fmt.Fprintf(os.Stderr, "=== reftable pass SKIPPED (GL-132): this Git has no reftable backend:\n%s\n", diagnostic)
		return code
	}
	fmt.Fprintln(os.Stderr, "=== the same tests on the reftable ref backend (GL-132)")
	RefFormat = "reftable"
	return m.Run()
}

// CheckRefFormat fails t unless the repository at dir uses the ref backend of
// the current pass.
func (r *Repo) CheckRefFormat(dir string) {
	r.T.Helper()
	want := RefFormat
	if want == "" {
		want = "files"
	}
	if got := strings.TrimSpace(string(r.At(dir, "rev-parse", "--show-ref-format"))); got != want {
		r.T.Fatalf("%s: ref format %q, want %q", dir, got, want)
	}
}

// CheckFixtureRefFormats is a package's check that its fixtures, a New
// repository and a bare clone made with RefFormatArgs, use the ref backend of
// the current pass, so the reftable pass can't silently test the files backend
// again.
func CheckFixtureRefFormats(t *testing.T) {
	t.Helper()
	repo := New(t)
	repo.CheckRefFormat(repo.Root)
	repo.Commit()
	bare := filepath.Join(repo.Root, "clone.git")
	repo.Git(append([]string{"clone", "-q", "--bare"}, append(RefFormatArgs(), repo.Root, bare)...)...)
	repo.CheckRefFormat(bare)
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
	env := append(isolatedEnv(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@localhost", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@localhost", "GIT_TERMINAL_PROMPT=0")
	r := &Repo{t, root, env, executable}
	if RefFormat != "" {
		// A failure then names its pass: both passes run the same test names.
		t.Logf("fixture ref format: %s", RefFormat)
	}
	r.Git(append([]string{"init", "-q", "-b", "main", "--template="}, RefFormatArgs()...)...)
	r.LineEndings(r.Root)
	return r
}

// TempDir is t.TempDir without symlinks and, on Windows, in its long form:
// TEMP can name a folder by its 8.3 short name (RUNNER~1), and git-local
// names paths in their long form.
func TempDir(t testing.TB) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// PrivatePerm is the permission a private (0600) file reports: Windows has no
// permission bits, and Go reports a writable file there as 0666.
func PrivatePerm() os.FileMode {
	if runtime.GOOS == "windows" {
		return 0666
	}
	return 0600
}

// LineEndings turns off core.autocrlf in the repository at dir on Windows,
// where Git sets it in the system configuration that git-local reads: it
// converts the fixtures' line endings, and overlays refuse it, as Windows
// users of overlays must too. New does this; a clone needs it again.
func (r *Repo) LineEndings(dir string) {
	if runtime.GOOS == "windows" {
		r.At(dir, "config", "core.autocrlf", "false")
	}
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
	r.LineEndings(r.Root)
	return r
}

func (r *Repo) Git(args ...string) []byte { return r.At(r.Root, args...) }
func (r *Repo) At(cwd string, args ...string) []byte {
	r.T.Helper()
	return r.Input(cwd, "", args...)
}

// Input runs a fixture Git command in cwd with stdin, for input too long for
// a command line (Windows allows 32,767 characters).
func (r *Repo) Input(cwd, stdin string, args ...string) []byte {
	r.T.Helper()
	cmd := exec.Command(r.GitPath, args...)
	cmd.Dir = cwd
	cmd.Env = r.Env
	cmd.Stdin = strings.NewReader(stdin)
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
