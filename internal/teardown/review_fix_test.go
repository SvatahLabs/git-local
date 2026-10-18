package teardown_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// GL-121: a deinit interrupted between removing .git/gitlocal/locks and
// .git/gitlocal leaves an empty private state directory; a rerun removes it.
// Before, every deinit failed with "open gitlocal/locks/repository.lock: no
// such file or directory".
func TestDeinitRemovesAnEmptyPrivateStateDirectory(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	linked := interruptedDeinit(f)
	state := filepath.Join(root, ".git", "gitlocal")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if out := f.gl(linked, "deinit"); !strings.Contains(out, "gitlocal-removed-interrupted") {
		t.Fatalf("deinit doesn't name the state an earlier run set aside:\n%s", out)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatalf("deinit left %s: %v", state, err)
	}
	f.gl(root, "init")
	f.gl(linked, "init")
}

// GL-121: deinit from a worktree that was never initialized names only the
// worktrees whose identity belongs to the live state: deinit from one that
// holds an earlier state's identity refuses. Before, it named those too.
func TestNeverInitializedHintSkipsWorktreesOfAnOlderState(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	linked := interruptedDeinit(f)
	if err := os.Remove(filepath.Join(root, ".git", "gitlocal-workspace.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(linked, linked+".away"); err != nil {
		t.Fatal(err)
	}
	f.gl(root, "init")
	if err := os.Rename(linked+".away", linked); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	f.git(f.withTool, root, "worktree", "add", "-q", fresh, "-b", "fresh")
	out, err := f.run(f.withTool, fresh, f.bin, "deinit")
	if err == nil || !strings.Contains(out, "never initialized") || !strings.Contains(out, root) {
		t.Fatalf("deinit from a worktree that was never initialized: %v\n%s", err, out)
	}
	if resolved, err := filepath.EvalSymlinks(linked); err != nil || strings.Contains(out, resolved) || strings.Contains(out, linked) {
		t.Fatalf("deinit names a worktree of an older state, where it refuses:\n%s", out)
	}
}

// GL-121: deinit from a worktree whose identity file is damaged refuses
// before changing anything and names the way out: the content to restore,
// or another worktree to run it from, whose deinit removes the damaged file.
// Before, it refused with only the JSON error (and before checkIdentity,
// with "read initialized workspace (run git local init first)", which init
// couldn't do either).
func TestDeinitNamesTheWayOutOfADamagedIdentity(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	f.git(f.plain, root, "add", "app.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	linked := filepath.Join(t.TempDir(), "linked")
	f.git(f.withTool, root, "worktree", "add", "-q", linked, "-b", "linked")
	f.gl(linked, "init")
	identity := filepath.Join(root, ".git", "gitlocal-workspace.json")
	data, err := os.ReadFile(identity)
	if err != nil {
		t.Fatal(err)
	}
	write(t, identity, "{\"schemaVersion\": 1, \"repositoryId\": ")
	resolved, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.run(f.withTool, root, f.bin, "deinit")
	if err == nil || !strings.Contains(out, "can't be read") || !strings.Contains(out, "run git local deinit from "+resolved+", which removes it too") {
		t.Fatalf("deinit with a damaged identity: %v\n%s", err, out)
	}
	_, content, _ := strings.Cut(out, "replace its content with ")
	content, _, _ = strings.Cut(content, " (the workspace")
	var named, wrote map[string]any
	if json.Unmarshal([]byte(content), &named) != nil || json.Unmarshal(data, &wrote) != nil || !reflect.DeepEqual(named, wrote) {
		t.Fatalf("deinit names %q to restore, init wrote %q:\n%s", content, data, out)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git", "gitlocal", "repository.json")); err != nil {
		t.Fatalf("refused deinit removed the private state: %v", err)
	}
	f.gl(linked, "deinit")
	for _, name := range []string{identity, filepath.Join(root, ".git", "gitlocal")} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("deinit left %s: %v", name, err)
		}
	}
	f.gl(root, "init")
	// The content it names works.
	write(t, identity, "{")
	out, err = f.run(f.withTool, root, f.bin, "deinit")
	_, content, _ = strings.Cut(out, "replace its content with ")
	content, _, _ = strings.Cut(content, " (the workspace")
	if err == nil || content == "" {
		t.Fatalf("deinit with a damaged identity: %v\n%s", err, out)
	}
	write(t, identity, content+"\n")
	f.gl(root, "deinit")
}

// GL-121: after a deinit interrupted once the state was set aside, git local
// init in a worktree without an identity, while other worktrees hold the
// removed state's, refuses before creating anything and names deinit.
// Before, it created .git/gitlocal/locks and refused with "inconsistent
// identity at <admin>".
func TestInitElsewhereAfterInterruptedDeinitNamesDeinit(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	linked := interruptedDeinit(f)
	fresh := filepath.Join(t.TempDir(), "fresh")
	f.git(f.withTool, root, "worktree", "add", "-q", fresh, "-b", "fresh")
	state := filepath.Join(root, ".git", "gitlocal")
	out, err := f.run(f.withTool, fresh, f.bin, "init")
	if err == nil || !strings.Contains(out, "git local deinit") {
		t.Fatalf("init beside identities of the removed state: %v\n%s", err, out)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatalf("refused init created %s: %v", state, err)
	}
	// What a refused init of an earlier release left: init refuses there
	// too, naming deinit.
	if err := os.MkdirAll(filepath.Join(state, "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "locks", "repository.lock"), "")
	out, err = f.run(f.withTool, fresh, f.bin, "init")
	if err == nil || !strings.Contains(out, "git local deinit") {
		t.Fatalf("init beside identities of the removed state, lock only: %v\n%s", err, out)
	}
	f.gl(fresh, "deinit")
	f.gl(fresh, "init")
	f.gl(linked, "init")
	f.gl(root, "init")
	f.gl(linked, "status")
}
