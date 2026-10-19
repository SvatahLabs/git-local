package teardown_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitlocal/internal/backup"
	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
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
	build := exec.Command("go", "build", "-o", bin, "../../cmd/git-local")
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

// backupStage packs the private state of repo's main checkout into an
// encrypted backup and decrypts it into a staged restore.
func backupStage(t *testing.T, repo *testutil.Repo) string {
	t.Helper()
	ctx := context.Background()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	var ref string
	if err := configtx.ReadSelection(ctx, r, repo.Root, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		ref = "refs/gitlocal/backups/" + w.RepositoryID + "/20260101T000000Z-00000000-0000-4000-8000-000000000001"
		if err := backup.PackEncrypted(ctx, filepath.Join(w.Common, "gitlocal"), w.Admin, &encrypted, key, ref); err != nil {
			return err
		}
		return recheck()
	}); err != nil {
		t.Fatal(err)
	}
	stage, err := backup.DecryptStaged(ctx, bytes.NewReader(encrypted.Bytes()), key, ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stage) })
	return stage
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

// GL-068: after deinit --unignore, Git behaves as if git-local was never
// installed, in the main and the linked worktree, even without git-local on
// PATH; the private history is kept. (Without --unignore the selected files
// stay ignored: TestDeinitKeepsSelectedPathsIgnored.)
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
	// Staging the file while the filter runs caches its size and time next
	// to the upstream blob; without the filter Git would trust that cache and
	// miss the local value, so deinit must make Git read the file again. The
	// file is dated back first so Git doesn't treat the entry as racily clean
	// (written in the same second as the index), which would hide the problem.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "config.txt"), past, past); err != nil {
		t.Fatal(err)
	}
	f.git(f.withTool, root, "add", "config.txt")
	if out := f.git(f.withTool, root, "status", "--porcelain", "--", "config.txt"); out != "" {
		t.Fatalf("overlay file shows as changed while active:\n%s", out)
	}
	out := f.gl(root, "deinit", "--unignore")
	if !strings.Contains(out, "gitlocal-removed-") {
		t.Fatalf("deinit output doesn't name the kept history:\n%s", out)
	}
	if !strings.Contains(out, "sensitive paths .env.local are no longer ignored") {
		t.Fatalf("deinit doesn't warn that the sensitive file is no longer ignored:\n%s", out)
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
	// Running it again finds nothing left to do, and names the kept state.
	if out := f.gl(root, "deinit"); !strings.Contains(out, "an earlier git local deinit set aside") {
		t.Fatalf("rerun doesn't name the kept state:\n%s", out)
	}
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
	stage := backupStage(t, g.repo)
	g.gl(root, "deinit", "--remove-history")
	if leftovers, _ := filepath.Glob(filepath.Join(root, ".git", "gitlocal*")); len(leftovers) != 0 {
		t.Fatalf("--remove-history left %q", leftovers)
	}
	if status := g.git(g.plain, root, "status", "--porcelain"); status != "" {
		t.Fatalf("status after deinit --remove-history:\n%s", status)
	}
	// Nor in a repository restored from a backup, whose restore lock is in
	// the common Git directory.
	r, err := gitexec.New(g.repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := backup.AdoptStaged(context.Background(), r, root, stage); err != nil {
		t.Fatalf("backup restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "gitlocal-backup-restore.lock")); err != nil {
		t.Fatalf("backup restore left no lock: %v", err)
	}
	g.gl(root, "deinit", "--remove-history")
	if leftovers, _ := filepath.Glob(filepath.Join(root, ".git", "gitlocal*")); len(leftovers) != 0 {
		t.Fatalf("--remove-history after a backup restore left %q", leftovers)
	}

	// A retired worktree's history is the only copy of its files.
	h := newFixture(t)
	root = h.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	h.git(h.plain, root, "add", "app.txt")
	h.git(h.plain, root, "commit", "-qm", "initial")
	h.gl(root, "init")
	linked := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-retired")
	h.git(h.withTool, root, "worktree", "add", "-q", linked, "-b", "retired")
	t.Cleanup(func() { os.RemoveAll(linked) })
	h.gl(linked, "init")
	write(t, filepath.Join(linked, "notes.local"), "only here\n")
	h.gl(linked, "track", "notes.local")
	h.gl(linked, "commit", "-m", "linked notes")
	h.gl(root, "workspace", "remove", "--force", linked)
	if out, err := h.run(h.withTool, root, h.bin, "deinit", "--remove-history"); err == nil || !strings.Contains(out, "retired workspace") || !strings.Contains(out, "without --remove-history") {
		t.Fatalf("deinit --remove-history with a retired workspace: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "gitlocal")); err != nil {
		t.Fatalf("refused deinit changed the private state: %v", err)
	}
}

// The filter writes the saved patch applied to the file's current upstream
// version, so after upstream changed other lines the working file differs
// from the saved local version without holding unsaved changes; deinit
// accepts it and keeps that content.
func TestDeinitAcceptsOverlayFileAfterUpstreamChange(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "config.txt"), "a=1\nb=2\nc=3\nd=4\nmode=shared\n")
	f.git(f.plain, root, "add", "config.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	write(t, filepath.Join(root, "config.txt"), "a=1\nb=2\nc=3\nd=4\nmode=local\n")
	f.gl(root, "overlay", "add", "local-mode", "--", "config.txt")
	// Upstream changes another line; checking it out runs the filter.
	f.git(f.withTool, root, "checkout", "-q", "-b", "upstream")
	write(t, filepath.Join(root, "config.txt"), "a=10\nb=2\nc=3\nd=4\nmode=local\n")
	f.git(f.withTool, root, "commit", "-qam", "upstream change")
	f.git(f.withTool, root, "checkout", "-q", "main")
	f.git(f.withTool, root, "merge", "-q", "--ff-only", "upstream")
	const merged = "a=10\nb=2\nc=3\nd=4\nmode=local\n"
	if data, err := os.ReadFile(filepath.Join(root, "config.txt")); err != nil || string(data) != merged {
		t.Fatalf("overlay file after the upstream change: %q %v", data, err)
	}
	f.gl(root, "deinit")
	if data, err := os.ReadFile(filepath.Join(root, "config.txt")); err != nil || string(data) != merged {
		t.Fatalf("overlay file after deinit: %q %v", data, err)
	}
	if got := f.git(f.plain, root, "show", "HEAD:config.txt"); got != "a=10\nb=2\nc=3\nd=4\nmode=shared\n" {
		t.Fatalf("upstream version: %q", got)
	}
	if status := f.git(f.plain, root, "status", "--porcelain"); status != " M config.txt\n" {
		t.Fatalf("status after deinit:\n%s", status)
	}
}

// GL-112: an overlay add interrupted before its manifest, whose directory the
// user then deleted as told, leaves the filter's include, required key and
// attributes block with no overlay; deinit removes them, so Git works on the
// path afterwards.
func TestDeinitRemovesFilterLeftByInterruptedOverlayAdd(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "config.txt"), "mode=shared\n")
	f.git(f.plain, root, "add", "config.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	write(t, filepath.Join(root, "config.txt"), "mode=local\n")
	f.gl(root, "overlay", "add", "local-mode", "--", "config.txt")
	if err := os.RemoveAll(filepath.Join(root, ".git", "gitlocal", "overlays")); err != nil {
		t.Fatal(err)
	}
	out := f.gl(root, "deinit")
	common := filepath.Join(root, ".git")
	for _, name := range []string{"info/attributes", "config"} {
		if data, _ := os.ReadFile(filepath.Join(common, name)); bytes.Contains(data, []byte("git-local")) || bytes.Contains(data, []byte("gitlocal")) {
			t.Fatalf("%s still mentions git-local:\n%s", name, data)
		}
	}
	if status := f.git(f.plain, root, "status", "--porcelain"); status != " M config.txt\n" {
		t.Fatalf("status after deinit:\n%s", status)
	}
	f.git(f.plain, root, "add", "config.txt")
	if !strings.Contains(out, "interrupted git local overlay add") {
		t.Fatalf("deinit doesn't report the filter it removed:\n%s", out)
	}
}

// GL-112: checks that can refuse run before deinit changes anything, so a
// refusal leaves the hooks and private state in place.
func TestDeinitRefusesBeforeChangingAnything(t *testing.T) {
	for _, kind := range []string{"edited-exclude-block", "uninitialized-worktree", "uninitialized-worktree-remove-history"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			root := f.repo.Root
			write(t, filepath.Join(root, "app.txt"), "application\n")
			f.git(f.plain, root, "add", "app.txt")
			f.git(f.plain, root, "commit", "-qm", "initial")
			f.gl(root, "init")
			write(t, filepath.Join(root, "notes.local"), "notes\n")
			f.gl(root, "track", "notes.local")
			f.gl(root, "hook", "install")
			dir, want := root, "info/exclude"
			switch kind {
			case "edited-exclude-block":
				path := filepath.Join(root, ".git", "info", "exclude")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				edited := strings.Replace(string(data), "notes.local", "notes.local\n/edited-by-hand", 1)
				if edited == string(data) {
					t.Fatalf("no managed line to edit:\n%s", data)
				}
				write(t, path, edited)
			case "uninitialized-worktree", "uninitialized-worktree-remove-history":
				dir = filepath.Join(filepath.Dir(root), filepath.Base(root)+"-plain")
				f.git(f.plain, root, "worktree", "add", "-q", dir, "-b", "plain")
				t.Cleanup(func() { os.RemoveAll(dir) })
				want = "run git local deinit from " + root
			}
			args := []string{"deinit"}
			if strings.HasSuffix(kind, "remove-history") {
				args = append(args, "--remove-history")
			}
			out, err := f.run(f.withTool, dir, f.bin, args...)
			if err == nil || !strings.Contains(out, want) || dir != root && strings.Contains(out, dir) {
				t.Fatalf("deinit: %v\n%s", err, out)
			}
			if hooks, _ := os.ReadFile(filepath.Join(root, ".git", "hooks", "pre-commit")); !bytes.Contains(hooks, []byte("git-local")) {
				t.Fatal("refused deinit removed the guards")
			}
			if _, err := os.Stat(filepath.Join(root, ".git", "gitlocal")); err != nil {
				t.Fatalf("refused deinit changed the private state: %v", err)
			}
		})
	}
}

// overlayFixture commits two files and saves an overlay of each.
func overlayFixture(t *testing.T) *fixture {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "a.txt"), "mode=shared\n")
	write(t, filepath.Join(root, "b.txt"), "mode=shared\n")
	f.git(f.plain, root, "add", "a.txt", "b.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	write(t, filepath.Join(root, "a.txt"), "mode=local\n")
	f.gl(root, "overlay", "add", "first", "--", "a.txt")
	write(t, filepath.Join(root, "b.txt"), "mode=local\n")
	f.gl(root, "overlay", "add", "second", "--", "b.txt")
	return f
}

// GL-118: the hook records are checked before the first overlay is removed,
// so a refusal leaves every overlay in place.
func TestDeinitChecksHookRecordsBeforeRemovingOverlays(t *testing.T) {
	f := overlayFixture(t)
	root := f.repo.Root
	f.gl(root, "hook", "install")
	write(t, filepath.Join(root, ".git", "gitlocal", "hook-install.json"), "damaged\n")
	before := f.git(f.withTool, root, "status", "--porcelain")
	config, _ := os.ReadFile(filepath.Join(root, ".git", "config"))
	if out, err := f.run(f.withTool, root, f.bin, "deinit"); err == nil {
		t.Fatalf("deinit accepted a damaged hook record:\n%s", out)
	}
	attributes, _ := os.ReadFile(filepath.Join(root, ".git", "info", "attributes"))
	for _, name := range []string{"a.txt", "b.txt"} {
		if !bytes.Contains(attributes, []byte(name)) {
			t.Fatalf("refused deinit removed the overlay of %s:\n%s", name, attributes)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(root, ".git", "config")); !bytes.Equal(after, config) || !bytes.Contains(after, []byte("gitlocal")) {
		t.Fatalf("refused deinit changed the overlay filter config:\n%s", after)
	}
	if status := f.git(f.withTool, root, "status", "--porcelain"); status != before {
		t.Fatalf("status changed by a refused deinit:\n%s\nbefore:\n%s", status, before)
	}
}

// GL-118: with the overlay installation record gone, the last overlay can't
// tell which filter settings existed before; deinit removes them as
// git-local's and says so, instead of stopping after the first overlay with
// nothing left that could finish it.
func TestDeinitFinishesWithoutOverlayInstallationRecord(t *testing.T) {
	f := overlayFixture(t)
	root := f.repo.Root
	if err := os.Remove(filepath.Join(root, ".git", "gitlocal", "overlay-install.json")); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(f.withTool, root, f.bin, "deinit")
	if err != nil || !strings.Contains(out, "overlay-install.json was missing") {
		t.Fatalf("deinit without the installation record: %v\n%s", err, out)
	}
	for _, name := range []string{"info/attributes", "config"} {
		if data, _ := os.ReadFile(filepath.Join(root, ".git", name)); bytes.Contains(data, []byte("gitlocal")) || bytes.Contains(data, []byte("git-local")) {
			t.Fatalf("%s still mentions git-local:\n%s", name, data)
		}
	}
	if status := f.git(f.plain, root, "status", "--porcelain"); status != " M a.txt\n M b.txt\n" {
		t.Fatalf("status after deinit:\n%s", status)
	}
}

// GL-118: removing the filter an interrupted overlay add left keeps a line
// the user wrote inside the managed attributes block (it refuses instead),
// and makes Git read the file it no longer filters again.
func TestDeinitStaleFilterKeepsUserLinesAndRenewsTimes(t *testing.T) {
	for _, kind := range []string{"user-line", "times"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			root := f.repo.Root
			write(t, filepath.Join(root, "config.txt"), "mode=shared\n")
			f.git(f.plain, root, "add", "config.txt")
			f.git(f.plain, root, "commit", "-qm", "initial")
			f.gl(root, "init")
			write(t, filepath.Join(root, "config.txt"), "mode=local\n")
			f.gl(root, "overlay", "add", "local-mode", "--", "config.txt")
			// Staging while the filter runs caches the file's size and time
			// next to the upstream blob (see TestDeinitLeavesAPlainRepository).
			past := time.Now().Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(root, "config.txt"), past, past); err != nil {
				t.Fatal(err)
			}
			f.git(f.withTool, root, "add", "config.txt")
			if err := os.RemoveAll(filepath.Join(root, ".git", "gitlocal", "overlays")); err != nil {
				t.Fatal(err)
			}
			attributes := filepath.Join(root, ".git", "info", "attributes")
			if kind == "user-line" {
				data, err := os.ReadFile(attributes)
				if err != nil {
					t.Fatal(err)
				}
				edited := strings.Replace(string(data), " filter=gitlocal-overlay\n", " filter=gitlocal-overlay\n*.bin binary\n", 1)
				write(t, attributes, edited)
				out, err := f.run(f.withTool, root, f.bin, "deinit")
				if err == nil || !strings.Contains(out, "was edited") {
					t.Fatalf("deinit dropped a user line in the managed block: %v\n%s", err, out)
				}
				if data, _ := os.ReadFile(attributes); string(data) != edited {
					t.Fatalf("refused deinit changed info/attributes:\n%s", data)
				}
				return
			}
			f.gl(root, "deinit")
			if status := f.git(f.plain, root, "status", "--porcelain"); status != " M config.txt\n" {
				t.Fatalf("Git doesn't see the local value after the filter's removal:\n%s", status)
			}
		})
	}
}

// With no managed attributes block left, removing a stale filter doesn't
// need the installation record (only the block's removal does), so a damaged
// record doesn't stop deinit; a missing one never did.
func TestDeinitStaleFilterWithoutBlockIgnoresDamagedRecord(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "config.txt"), "mode=shared\n")
	f.git(f.plain, root, "add", "config.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	write(t, filepath.Join(root, "config.txt"), "mode=local\n")
	f.gl(root, "overlay", "add", "local-mode", "--", "config.txt")
	// An overlay add interrupted before its manifest and attributes block.
	for _, name := range []string{"gitlocal/overlays", "info/attributes"} {
		if err := os.RemoveAll(filepath.Join(root, ".git", filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, ".git", "gitlocal", "overlay-install.json"), "damaged\n")
	if out, err := f.run(f.withTool, root, f.bin, "deinit"); err != nil {
		t.Fatalf("deinit with a stale filter and a damaged installation record: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(root, ".git", "config")); bytes.Contains(data, []byte("gitlocal")) {
		t.Fatalf("config still mentions git-local:\n%s", data)
	}
}

// The warning about filter settings that existed before overlays were
// installed names only those deinit removed.
func TestDeinitNamesOnlyThePresetFilterSettingItRemoved(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "config.txt"), "mode=shared\n")
	f.git(f.plain, root, "add", "config.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.git(f.plain, root, "config", "filter.gitlocal-overlay.required", "true")
	f.gl(root, "init")
	write(t, filepath.Join(root, "config.txt"), "mode=local\n")
	f.gl(root, "overlay", "add", "local-mode", "--", "config.txt")
	out, err := f.run(f.withTool, root, f.bin, "deinit")
	if err != nil || !strings.Contains(out, "filter.gitlocal-overlay.required was set before overlays were installed; deinit removed it") || strings.Contains(out, "include.path gitlocal/overlay-filter.gitconfig and") {
		t.Fatalf("deinit with a preset required key: %v\n%s", err, out)
	}
}

// After git worktree move, deinit from a worktree where git local was never
// initialized names where the initialized worktree is now: Git updates its
// gitdir file, while its workspace record keeps the root it had at init.
func TestDeinitNamesMovedWorktree(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	f.git(f.plain, root, "add", "app.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	before := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-before")
	after := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-after")
	plain := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-plain")
	t.Cleanup(func() {
		for _, dir := range []string{before, after, plain} {
			os.RemoveAll(dir)
		}
	})
	f.git(f.plain, root, "worktree", "add", "-q", before, "-b", "moved")
	f.gl(before, "init")
	f.git(f.plain, root, "worktree", "move", before, after)
	f.git(f.plain, root, "worktree", "add", "-q", plain, "-b", "plain")
	out, err := f.run(f.withTool, plain, f.bin, "deinit")
	if err == nil || !strings.Contains(out, "run git local deinit from ") || !strings.Contains(out, after) || strings.Contains(out, before) {
		t.Fatalf("deinit from an uninitialized worktree after a worktree move: %v\n%s", err, out)
	}
}

// GL-118: from a worktree git local init never ran in, deinit names the
// initialized worktree by its recorded root, also when the Git directory is
// separate from the main worktree (--separate-git-dir).
func TestDeinitNamesInitializedWorktreeWithSeparateGitDir(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	f.git(f.plain, root, "add", "app.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	separate := filepath.Join(t.TempDir(), "repo.git")
	f.git(f.plain, root, "init", "-q", "--separate-git-dir", separate)
	f.gl(root, "init")
	plain := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-plain")
	f.git(f.plain, root, "worktree", "add", "-q", plain, "-b", "plain")
	t.Cleanup(func() { os.RemoveAll(plain) })
	out, err := f.run(f.withTool, plain, f.bin, "deinit")
	if err == nil || !strings.Contains(out, "run git local deinit from "+root) {
		t.Fatalf("deinit from an uninitialized worktree: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(separate, "gitlocal")); err != nil {
		t.Fatalf("refused deinit changed the private state: %v", err)
	}
}
