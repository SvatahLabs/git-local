package teardown_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// interruptedDeinit initializes the main worktree and a linked one, then
// leaves what a deinit interrupted right after setting the private state
// aside leaves: the state renamed, every identity file still there. It
// returns the linked worktree.
func interruptedDeinit(f *fixture) string {
	t, root := f.t, f.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	f.git(f.plain, root, "add", "app.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	linked := filepath.Join(t.TempDir(), "linked")
	f.git(f.withTool, root, "worktree", "add", "-q", linked, "-b", "linked")
	f.gl(linked, "init")
	if err := os.Rename(filepath.Join(root, ".git", "gitlocal"), filepath.Join(root, ".git", "gitlocal-removed-interrupted")); err != nil {
		t.Fatal(err)
	}
	return linked
}

// GL-121: after a deinit interrupted once the state was set aside, git local
// init in a worktree that still holds its identity refuses before creating
// anything and names deinit; a private state directory holding only its
// lock (what such an init left before) doesn't block deinit; and afterwards
// init works everywhere. Before, init left .git/gitlocal/locks, deinit
// failed on every run with "open gitlocal/repository.json: no such file or
// directory", and init refused in every worktree.
func TestInterruptedDeinitLeavesNothingThatBlocksDeinitOrInit(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	linked := interruptedDeinit(f)
	state := filepath.Join(root, ".git", "gitlocal")
	out, err := f.run(f.withTool, linked, f.bin, "init")
	if err == nil || !strings.Contains(out, "run git local deinit here") {
		t.Fatalf("init with an identity of the removed state: %v\n%s", err, out)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatalf("refused init created %s: %v", state, err)
	}
	// What a refused init of an earlier release left.
	if err := os.MkdirAll(filepath.Join(state, "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "locks", "repository.lock"), "")
	if out := f.gl(linked, "deinit"); !strings.Contains(out, "gitlocal-removed-interrupted") {
		t.Fatalf("deinit doesn't name the state an earlier run set aside:\n%s", out)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatalf("deinit left %s: %v", state, err)
	}
	for _, identity := range []string{filepath.Join(root, ".git", "gitlocal-workspace.json"), filepath.Join(root, ".git", "worktrees", "linked", "gitlocal-workspace.json")} {
		if _, err := os.Lstat(identity); !os.IsNotExist(err) {
			t.Fatalf("deinit left %s: %v", identity, err)
		}
	}
	f.gl(root, "init")
	f.gl(linked, "init")
	f.gl(linked, "status")
}

// GL-121: a deinit interrupted after removing some identities, then git local
// init in a worktree without one while another worktree was away, leaves that
// worktree with the identity of a state older than the live one. git local init there names the file
// to delete, deinit there refuses rather than remove the live state, and a
// deinit of the live state removes it too. Before, init refused with
// "workspace belongs to another or missing store", deinit there removed the
// live state, and the live state's deinit kept the old identity.
func TestIdentityOfAnOlderStateDoesntBlockInit(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	linked := interruptedDeinit(f)
	if err := os.Remove(filepath.Join(root, ".git", "gitlocal-workspace.json")); err != nil {
		t.Fatal(err)
	}
	// init checks other worktrees' identities only where they are; this one
	// is away (a removable disk, say) while the main worktree is initialized.
	if err := os.Rename(linked, linked+".away"); err != nil {
		t.Fatal(err)
	}
	f.gl(root, "init")
	if err := os.Rename(linked+".away", linked); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ".git", "worktrees", "linked", "gitlocal-workspace.json")
	out, err := f.run(f.withTool, linked, f.bin, "init")
	if err == nil || !strings.Contains(out, "belongs to an earlier private state") || !strings.Contains(out, "delete it, then run git local init again") {
		t.Fatalf("init with an older state's identity: %v\n%s", err, out)
	}
	out, err = f.run(f.withTool, linked, f.bin, "deinit")
	if err == nil || !strings.Contains(out, "belongs to an earlier private state") {
		t.Fatalf("deinit from a worktree of an older state: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git", "gitlocal", "repository.json")); err != nil {
		t.Fatalf("refused deinit removed the live state: %v", err)
	}
	f.gl(root, "status")
	f.gl(root, "deinit")
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("deinit kept an older state's identity: %v", err)
	}
	f.gl(linked, "init")
	f.gl(root, "init")
	f.gl(linked, "status")
}

// GL-121: deinit takes an overlay installation record it can't read like a
// missing one: it removes the filter as git-local's and says so. Before, it
// refused with "invalid overlay installation record" or a JSON error.
func TestDeinitTakesUnreadableInstallationRecordAsMissing(t *testing.T) {
	f := overlayFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, ".git", "gitlocal", "overlay-install.json"), "{\"schemaVersion\": 9}\n")
	out, err := f.run(f.withTool, root, f.bin, "deinit")
	if err != nil || !strings.Contains(out, "overlay-install.json couldn't be read") {
		t.Fatalf("deinit with an unreadable installation record: %v\n%s", err, out)
	}
	for _, name := range []string{"info/attributes", "config"} {
		if data, _ := os.ReadFile(filepath.Join(root, ".git", name)); strings.Contains(string(data), "gitlocal") || strings.Contains(string(data), "git-local") {
			t.Fatalf("%s still mentions git-local:\n%s", name, data)
		}
	}
	if status := f.git(f.plain, root, "status", "--porcelain"); status != " M a.txt\n M b.txt\n" {
		t.Fatalf("status after deinit:\n%s", status)
	}
}

// GL-121: once the stale filter an interrupted overlay add left is removed,
// a path whose times can't be renewed is a warning, not a failure: a rerun
// finds nothing left to do, so the failure would have hidden the step.
func TestDeinitWarnsWhenTimesCantBeRenewedAfterTheFilterIsGone(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user can't search")
	}
	f := newFixture(t)
	root := f.repo.Root
	if err := os.Mkdir(filepath.Join(root, "locked"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "locked", "config.txt"), "mode=shared\n")
	f.git(f.plain, root, "add", "locked/config.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	write(t, filepath.Join(root, "locked", "config.txt"), "mode=local\n")
	f.gl(root, "overlay", "add", "local-mode", "--", "locked/config.txt")
	if err := os.RemoveAll(filepath.Join(root, ".git", "gitlocal", "overlays")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "locked"), 0700) })
	out, err := f.run(f.withTool, root, f.bin, "deinit")
	if err != nil || !strings.Contains(out, "locked/config.txt") || !strings.Contains(out, "touch it") {
		t.Fatalf("deinit when times can't be renewed: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git", "gitlocal")); !os.IsNotExist(err) {
		t.Fatalf("deinit didn't finish: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, ".git", "info", "attributes")); strings.Contains(string(data), "gitlocal") {
		t.Fatalf("info/attributes still holds the filter:\n%s", data)
	}
}
