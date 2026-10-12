package teardown_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func testBinary(t *testing.T) string {
	t.Helper()
	name := "git-local"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if prebuilt := os.Getenv("GITLOCAL_TEST_BINARY"); prebuilt != "" {
		source, err := os.Open(prebuilt)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		destination, err := os.OpenFile(bin, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(destination, source); err != nil {
			t.Fatal(err)
		}
		if err := destination.Close(); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, "../../cmd/git-local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build git-local: %v\n%s", err, output)
	}
	return bin
}

type fixture struct {
	t        *testing.T
	repo     *testutil.Repo
	bin, key string
	// withTool runs with git-local on PATH; plain runs without it, as on a
	// machine where git-local was never installed.
	withTool, plain []string
}

func newFixture(t *testing.T) *fixture {
	repo := testutil.New(t)
	bin := testBinary(t)
	gitDir := filepath.Dir(repo.GitPath)
	f := &fixture{t: t, repo: repo, bin: bin, key: filepath.Join(t.TempDir(), "files.key")}
	base := repo.RunnerEnv()
	for _, entry := range base {
		if strings.HasPrefix(entry, "PATH=") {
			continue
		}
		f.withTool = append(f.withTool, entry)
		f.plain = append(f.plain, entry)
	}
	f.withTool = append(f.withTool, "PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+gitDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	f.plain = append(f.plain, "PATH="+gitDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	home := ""
	for _, entry := range base {
		if v, ok := strings.CutPrefix(entry, "HOME="); ok {
			home = v
		}
	}
	if home == "" || !strings.HasPrefix(home, repo.Root+string(filepath.Separator)) {
		t.Fatalf("fixture HOME isn't isolated: %q", home)
	}
	identity := []string{"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@localhost", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@localhost"}
	f.withTool = append(f.withTool, identity...)
	f.plain = append(f.plain, identity...)
	// The fixture HOME lives in the checkout; keep it out of status.
	if err := os.MkdirAll(filepath.Join(repo.Root, ".git", "info"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo.Root, ".git", "info", "exclude"), "/fixture-home/\n")
	return f
}

func (f *fixture) run(env []string, dir, name string, args ...string) (string, error) {
	f.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *fixture) gl(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.run(f.withTool, dir, f.bin, args...)
	if err != nil {
		f.t.Fatalf("git-local %v: %v\n%s", args, err, out)
	}
	return out
}

func (f *fixture) git(env []string, dir string, args ...string) string {
	f.t.Helper()
	out, err := f.run(env, dir, f.repo.GitPath, args...)
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// installEverything sets up selections, encryption, guards, snapshot hooks,
// an overlay and a linked worktree.
func (f *fixture) installEverything() string {
	t, root := f.t, f.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	write(t, filepath.Join(root, "config.txt"), "mode=shared\n")
	f.git(f.plain, root, "add", "app.txt", "config.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	write(t, filepath.Join(root, "notes.local"), "private notes\n")
	write(t, filepath.Join(root, ".env.local"), "TOKEN=secret\n")
	f.gl(root, "track", "notes.local", ".env.local")
	f.gl(root, "encrypt", "keygen", f.key)
	f.gl(root, "encrypt", "enable", "--key-file", f.key, "--", ".env.local")
	f.gl(root, "commit", "-m", "local files")
	f.gl(root, "hook", "install")
	f.gl(root, "auto", "install")
	write(t, filepath.Join(root, "config.txt"), "mode=local\n")
	f.gl(root, "overlay", "add", "local-mode", "--", "config.txt")
	linked := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-linked")
	f.git(f.withTool, root, "worktree", "add", "-q", linked, "-b", "linked")
	t.Cleanup(func() { os.RemoveAll(linked) })
	f.gl(linked, "init")
	return linked
}

// GL-068: after deinit, Git behaves as if git-local was never installed, in
// the main and the linked worktree, even without git-local on PATH; the
// private history is kept.
func TestDeinitLeavesAPlainRepository(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	linked := f.installEverything()
	// The overlay is active: the working file differs from HEAD only by the
	// patch the filter removes.
	if out, err := f.run(f.withTool, root, f.repo.GitPath, "diff", "--exit-code", "--", "config.txt"); err != nil {
		t.Fatalf("overlay inactive before deinit: %v\n%s", err, out)
	}
	// An overlay file edited since it was saved is refused.
	write(t, filepath.Join(root, "config.txt"), "mode=edited\n")
	if out, err := f.run(f.withTool, root, f.bin, "deinit"); err == nil || !strings.Contains(out, "config.txt") || !strings.Contains(out, "local-mode") {
		t.Fatalf("deinit with an unsaved overlay change: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "gitlocal")); err != nil {
		t.Fatalf("refused deinit changed the private state: %v", err)
	}
	write(t, filepath.Join(root, "config.txt"), "mode=local\n")
	out := f.gl(root, "deinit")
	if !strings.Contains(out, "gitlocal-removed-") {
		t.Fatalf("deinit output doesn't name the kept history:\n%s", out)
	}
	common := filepath.Join(root, ".git")
	if _, err := os.Stat(filepath.Join(common, "gitlocal")); !os.IsNotExist(err) {
		t.Fatalf("private state still in service: %v", err)
	}
	archives, _ := filepath.Glob(filepath.Join(common, "gitlocal-removed-*", "history.git", "config"))
	if len(archives) != 1 {
		t.Fatalf("kept history: %q", archives)
	}
	if _, err := os.Stat(f.key); err != nil {
		t.Fatalf("deinit touched the key file: %v", err)
	}
	for _, dir := range []string{common, filepath.Join(common, "worktrees", filepath.Base(linked))} {
		if _, err := os.Stat(filepath.Join(dir, "gitlocal-workspace.json")); !os.IsNotExist(err) {
			t.Fatalf("identity left in %s: %v", dir, err)
		}
	}
	hooks, _ := filepath.Glob(filepath.Join(common, "hooks", "*"))
	for _, hook := range hooks {
		if data, _ := os.ReadFile(hook); bytes.Contains(data, []byte("git-local")) {
			t.Fatalf("hook %s still runs git-local:\n%s", hook, data)
		}
	}
	for _, name := range []string{"info/exclude", "info/attributes", "config"} {
		if data, _ := os.ReadFile(filepath.Join(common, name)); bytes.Contains(data, []byte("git-local")) || bytes.Contains(data, []byte("gitlocal")) {
			t.Fatalf("%s still mentions git-local:\n%s", name, data)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(common, "info", "exclude")); string(data) != "/fixture-home/\n" {
		t.Fatalf("the user's exclude lines changed: %q", data)
	}
	// Without git-local on PATH: private files are untracked, the overlay's
	// file keeps its local value as an ordinary modification, and adding and
	// committing work in both worktrees.
	status := f.git(f.plain, root, "status", "--porcelain")
	for _, want := range []string{"?? notes.local", "?? .env.local", " M config.txt"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status after deinit lacks %q:\n%s", want, status)
		}
	}
	f.git(f.plain, root, "add", "notes.local", "config.txt")
	f.git(f.plain, root, "commit", "-qm", "plain commit")
	if got := f.git(f.plain, root, "show", "HEAD:config.txt"); got != "mode=local\n" {
		t.Fatalf("committed overlay file: %q", got)
	}
	write(t, filepath.Join(linked, "linked.txt"), "linked\n")
	f.git(f.plain, linked, "add", "linked.txt")
	f.git(f.plain, linked, "commit", "-qm", "linked commit")
	// The snapshot hook seeded the linked worktree with the local files; they
	// are now ordinary untracked files there too.
	if out := f.git(f.plain, linked, "status", "--porcelain"); out != "?? .env.local\n?? notes.local\n" {
		t.Fatalf("linked status after deinit:\n%s", out)
	}
	// Running it again finds nothing left to do.
	f.gl(root, "deinit")
}

// --remove-history deletes the private state only when no restore recovery
// ref or quarantine would be lost.
func TestDeinitRemoveHistoryRefusesRecoveryRefs(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	f.git(f.plain, root, "add", "app.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	note := filepath.Join(root, "notes.local")
	write(t, note, "v1\n")
	f.gl(root, "track", "notes.local")
	f.gl(root, "commit", "-m", "v1")
	f.gl(root, "hook", "install")
	write(t, note, "v2\n")
	f.gl(root, "restore", "--source", "latest", "--overwrite", "--", "notes.local")
	out, err := f.run(f.withTool, root, f.bin, "deinit", "--remove-history")
	if err == nil || !strings.Contains(out, "refs/gitlocal/recovery/") {
		t.Fatalf("deinit --remove-history with a recovery ref: %v\n%s", err, out)
	}
	if hooks, _ := os.ReadFile(filepath.Join(root, ".git", "hooks", "pre-commit")); !bytes.Contains(hooks, []byte("git-local")) {
		t.Fatal("refused deinit removed the guards")
	}
	f.gl(root, "deinit")
	if archives, _ := filepath.Glob(filepath.Join(root, ".git", "gitlocal-removed-*")); len(archives) != 1 {
		t.Fatalf("kept history: %q", archives)
	}

	g := newFixture(t)
	root = g.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	g.git(g.plain, root, "add", "app.txt")
	g.git(g.plain, root, "commit", "-qm", "initial")
	g.gl(root, "init")
	write(t, filepath.Join(root, "notes.local"), "v1\n")
	g.gl(root, "track", "notes.local")
	g.gl(root, "commit", "-m", "v1")
	g.gl(root, "deinit", "--remove-history")
	if leftovers, _ := filepath.Glob(filepath.Join(root, ".git", "gitlocal*")); len(leftovers) != 0 {
		t.Fatalf("--remove-history left %q", leftovers)
	}
	if status := g.git(g.plain, root, "status", "--porcelain"); status != "?? notes.local\n" {
		t.Fatalf("status after deinit --remove-history:\n%s", status)
	}
}
