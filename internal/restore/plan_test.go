package restore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

var ctx = context.Background()

func fixture(t *testing.T) (*testutil.Repo, *gitexec.Runner, *workspace.Context) {
	t.Helper()
	return fixtureFrom(t, testutil.New(t))
}

// fixtureFrom commits to repo and initializes git local in it.
func fixtureFrom(t *testing.T, repo *testutil.Repo) (*testutil.Repo, *gitexec.Runner, *workspace.Context) {
	t.Helper()
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	return repo, r, w
}

func TestEncryptedSnapshotRestoreUsesProtectedJournal(t *testing.T) {
	repo, r, w := fixture(t)
	write(t, repo.Root, ".env.local", "TOKEN=private\n")
	track(t, r, repo.Root, true, ".env.local")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	source := save(t, r, repo.Root)
	write(t, repo.Root, ".env.local", "TOKEN=changed\n")
	plan, err := Plan(ctx, r, repo.Root, "", "latest", []string{".env.local"}, true)
	if err != nil || len(plan.Operations()) != 1 || plan.Operations()[0].Before.KeyedDigest == "" || plan.Operations()[0].Before.BlobID != "" || plan.Operations()[0].Before.SHA256 != "" {
		t.Fatalf("encrypted restore preview leaked unprotected evidence: %+v %v", plan.Operations(), err)
	}
	data, err := plan.JSON()
	if err != nil || strings.Contains(string(data), "TOKEN=") || strings.Contains(string(data), "sourcePlainBlob") {
		t.Fatalf("encrypted restore JSON leaked plaintext evidence: %v", err)
	}
	result, err := Execute(ctx, r, repo.Root, "", "latest", []string{".env.local"}, true)
	if err != nil || !result.Changed {
		t.Fatalf("encrypted restore: %+v %v", result, err)
	}
	working, err := os.ReadFile(filepath.Join(repo.Root, ".env.local"))
	if err != nil || string(working) != "TOKEN=private\n" {
		t.Fatalf("restore failed to install source: %q %v", working, err)
	}
	meta, err := openMeta(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	j, err := readJournal(meta, completedPath(result.TransactionID))
	if err != nil || j.SchemaVersion != 2 || len(j.Manifest.Plan.Operations) != 1 || len(j.Manifest.Post) != 1 || len(j.Manifest.BeforeStored) != 1 || len(j.Manifest.PostStored) != 1 || j.Manifest.Plan.Operations[0].Before.KeyedDigest == "" || j.Manifest.Plan.Operations[0].Before.BlobID != "" || j.Manifest.Post[0].KeyedDigest == "" || j.Manifest.Post[0].BlobID != "" {
		t.Fatalf("encrypted restore journal contains unprotected fingerprints: %+v %v", j, err)
	}
	journalBytes, err := meta.ReadFile(completedPath(result.TransactionID), journalLimit)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := history.Resolve(ctx, r, repo.Root, w.WorkspaceID, source)
	if err != nil {
		t.Fatal(err)
	}
	plainID, err := history.PlainBlobIDLocked(ctx, r, *w, snapshot, ".env.local")
	if err != nil || strings.Contains(string(journalBytes), plainID) {
		t.Fatalf("journal contains plaintext source object ID: %v", err)
	}
	beforeID, err := r.History(ctx, w.Store, strings.NewReader("TOKEN=changed\n"), "hash-object", "--stdin", "--no-filters")
	if err != nil || strings.Contains(string(journalBytes), strings.TrimSpace(string(beforeID))) {
		t.Fatalf("journal contains plaintext preimage object ID: %v", err)
	}
	for _, secret := range []string{"TOKEN=private\n", "TOKEN=changed\n"} {
		plainDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(secret)))
		if strings.Contains(string(journalBytes), plainDigest) {
			t.Fatal("journal contains unkeyed content digest")
		}
	}
	for _, id := range []string{j.Manifest.BeforeStored[0], j.Manifest.PostStored[0]} {
		raw, e := r.History(ctx, w.Store, nil, "cat-file", "blob", id)
		if e != nil || strings.Contains(string(raw), "TOKEN=") {
			t.Fatalf("recovery Git blob contains plaintext: %v", e)
		}
	}
}
func write(t *testing.T, root, p, value string) {
	t.Helper()
	p = filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func track(t *testing.T, r *gitexec.Runner, root string, on bool, p string) {
	t.Helper()
	if _, err := configtx.Change(ctx, r, root, on, p); err != nil {
		t.Fatal(err)
	}
}
func save(t *testing.T, r *gitexec.Runner, root string) string {
	t.Helper()
	v, err := history.Commit(ctx, r, root, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}
func remove(t *testing.T, root, p string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, p)); err != nil {
		t.Fatal(err)
	}
}
func durable(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		// Directory mtimes change in ordinary setup; retain them too to detect dry-run writes.
		var data []byte
		if fi.Mode()&os.ModeSymlink != 0 {
			v, e := os.Readlink(p)
			if e != nil {
				return e
			}
			data = []byte(v)
		} else if !d.IsDir() {
			data, err = os.ReadFile(p)
			if err != nil {
				return err
			}
		}
		result[p] = fmt.Sprintf("%v:%v:%s", fi.Mode(), fi.ModTime().UnixNano(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestPreciseReadOnlyPlan(t *testing.T) {
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "old")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "new")
	write(t, root, "notes/b", "later")
	save(t, r, root)
	write(t, root, "notes/unknown", "unrecorded")
	if err := os.Chmod(filepath.Join(root, "notes/a"), 0640); err != nil {
		t.Fatal(err)
	}
	before := durable(t, root)
	p, err := Plan(ctx, r, root, "", source, []string{"notes/b", "notes", "notes/a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ops := p.Operations()
	if len(ops) != 2 || ops[0].Path != "notes/a" || ops[0].Action != "replace" || ops[1].Path != "notes/b" || ops[1].Action != "delete" {
		t.Fatalf("operations: %+v", ops)
	}
	if runtime.GOOS != "windows" && ops[0].Permissions != 0640 {
		t.Fatal(ops[0])
	}
	if ops[0].Before.SHA256 == "" || ops[0].SourceBlob == "" || ops[0].Dirty {
		t.Fatal(ops[0])
	}
	again, err := Plan(ctx, r, root, "", source, []string{"notes"}, false)
	if err != nil || !p.Equal(again) {
		t.Fatal("unstable serialization", err)
	}
	ops[0].Path = "../escape"
	if p.Operations()[0].Path != "notes/a" {
		t.Fatal("mutable plan")
	}
	data, err := p.JSON()
	if err != nil || strings.Contains(string(data), root) || strings.Contains(string(data), "../escape") {
		t.Fatal("unsafe serialized plan", err)
	}
	noop, err := Plan(ctx, r, root, "", "latest", []string{"notes"}, false)
	if err != nil || len(noop.Operations()) != 0 {
		t.Fatal("latest matching tree should be a no-op", noop, err)
	}
	if !reflect.DeepEqual(before, durable(t, root)) {
		t.Fatal("dry run changed durable state")
	}
}
func TestDirtyAndCleanedWorkingTree(t *testing.T) {
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/sub/a", "old")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/sub/a", "new")
	save(t, r, root)
	write(t, root, "notes/sub/a", "dirty")
	if _, err := Plan(ctx, r, root, "", source, []string{"notes"}, false); err == nil || !strings.Contains(err.Error(), "--overwrite") {
		t.Fatal(err)
	}
	p, err := Plan(ctx, r, root, "", source, []string{"notes"}, true)
	if err != nil || len(p.Operations()) != 1 || !p.Operations()[0].Dirty {
		t.Fatal(p, err)
	}
	remove(t, root, "notes")
	p, err = Plan(ctx, r, root, "", source, []string{"notes"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.data.CreateDirectories, []string{"notes", "notes/sub"}) || p.Operations()[0].Action != "create" || p.Operations()[0].Permissions != 0600 || !p.Operations()[0].Dirty {
		t.Fatal(p)
	}
}
func TestDeletedSourceAndExclusions(t *testing.T) {
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "saved")
	track(t, r, root, true, "notes")
	save(t, r, root)
	remove(t, root, "notes/a")
	source := save(t, r, root)
	write(t, root, "notes/a", "new")
	write(t, root, "notes/b", "new")
	save(t, r, root)
	track(t, r, root, false, "notes/b")
	write(t, root, "notes/unknown", "keep")
	p, err := Plan(ctx, r, root, "", source, []string{"notes"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Operations()) != 1 || p.Operations()[0].Path != "notes/a" || p.Operations()[0].Action != "delete" {
		t.Fatal(p)
	}
	if _, err = Plan(ctx, r, root, "", source, []string{"notes/b"}, true); err == nil {
		t.Fatal("deselected explicit path accepted")
	}
}
func TestCrossWorkspaceWithoutBaseline(t *testing.T) {
	repo, r, w := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "saved")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	destination := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "linked", destination)
	dw, err := workspace.Init(ctx, r, destination)
	if err != nil {
		t.Fatal(err)
	}
	write(t, destination, "notes/unknown", "keep")
	track(t, r, destination, true, "notes")
	before := durable(t, root)
	destBefore := durable(t, destination)
	p, err := Plan(ctx, r, destination, w.WorkspaceID, source, []string{"notes"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Operations()) != 1 || p.Operations()[0].Action != "create" || p.data.DestinationBaseline != "" || p.data.DestinationWorkspaceID != dw.WorkspaceID {
		t.Fatal(p)
	}
	if !reflect.DeepEqual(before, durable(t, root)) || !reflect.DeepEqual(destBefore, durable(t, destination)) {
		t.Fatal("import plan mutated state")
	}
	write(t, destination, "notes/a", "unsaved obstruction")
	if _, err = Plan(ctx, r, destination, w.WorkspaceID, source, []string{"notes"}, true); err == nil || !strings.Contains(err.Error(), "unsaved obstruction") {
		t.Fatal(err)
	}
	remove(t, destination, "notes/a")
	write(t, destination, "notes/a/tracked", "main owned descendant")
	repo.At(destination, "add", "-f", "notes/a/tracked")
	if _, err = Plan(ctx, r, destination, w.WorkspaceID, source, []string{"notes"}, true); err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatal("main-owned descendant was not rejected before file reads", err)
	}
}
func TestTopologyAndOwnedObstructions(t *testing.T) {
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "file")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	remove(t, root, "notes/a")
	write(t, root, "notes/a/child", "child")
	save(t, r, root)
	remove(t, root, "notes")
	if _, err := Plan(ctx, r, root, "", source, []string{"notes"}, true); err == nil || !strings.Contains(err.Error(), "transition") {
		t.Fatal(err)
	}
	// Excluding the historical child must not hide the directory transition.
	track(t, r, root, false, "notes/a/child")
	if _, err := Plan(ctx, r, root, "", source, []string{"notes/a"}, true); err == nil || !strings.Contains(err.Error(), "transition") {
		t.Fatal(err)
	}
	// A main-owned descendant blocks a historical file even after the tree is cleaned.
	write(t, root, "notes/a/tracked", "main")
	repo.Git("add", "-f", "notes/a/tracked")
	remove(t, root, "notes")
	if _, err := Plan(ctx, r, root, "", source, []string{"notes/a"}, true); err == nil {
		t.Fatal("owned obstruction accepted")
	}
}
func TestBoundaryObstructions(t *testing.T) {
	for _, kind := range []string{"directory", "symlink-parent", "hardlink", "alias", "nested-repo", "main-owned"} {
		t.Run(kind, func(t *testing.T) {
			repo, r, _ := fixture(t)
			root := repo.Root
			write(t, root, "notes/a", "source")
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			write(t, root, "notes/a", "baseline")
			save(t, r, root)
			switch kind {
			case "directory":
				remove(t, root, "notes/a")
				write(t, root, "notes/a/unknown", "keep")
			case "symlink-parent":
				remove(t, root, "notes")
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "notes")); err != nil {
					t.Skip(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(root, "notes/a"), filepath.Join(root, "alias")); err != nil {
					t.Skip(err)
				}
			case "alias":
				remove(t, root, "notes/a")
				write(t, root, "notes/A", "alias")
			case "nested-repo":
				repo.At(filepath.Join(root, "notes"), "init", "-q", "--template=")
			case "main-owned":
				repo.Git("add", "-f", "notes/a")
			}
			if _, err := Plan(ctx, r, root, "", source, []string{"notes"}, true); err == nil {
				t.Fatal("obstruction accepted")
			}
		})
	}
}
func TestLateChangesFail(t *testing.T) {
	for _, kind := range []string{"content", "ownership", "parent", "configuration"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "parent" && runtime.GOOS == "windows" {
				t.Skip("POSIX directory permissions")
			}
			repo, r, _ := fixture(t)
			root := repo.Root
			write(t, root, "notes/a", "old")
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			write(t, root, "notes/a", "new")
			save(t, r, root)
			_, err := plan(ctx, r, root, "", source, []string{"notes"}, true, func() error {
				switch kind {
				case "content":
					write(t, root, "notes/a", "changed")
				case "ownership":
					repo.Git("add", "-f", "notes/a")
				case "configuration":
					f, e := os.OpenFile(filepath.Join(root, ".git/info/exclude"), os.O_APPEND|os.O_WRONLY, 0)
					if e != nil {
						return e
					}
					_, e = f.WriteString("\n# external change\n")
					closeErr := f.Close()
					if e != nil {
						return e
					}
					return closeErr
				case "parent":
					return os.Chmod(filepath.Join(root, "notes"), 0755)
				}
				return nil
			})
			if err == nil {
				t.Fatal("late change accepted")
			}
		})
	}
}
func TestSymlinkAndExecutablePlans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode and symlink fixture")
	}
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/run", "old")
	if err := os.Chmod(filepath.Join(root, "notes/run"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(root, "notes/link")); err != nil {
		t.Skip(err)
	}
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	remove(t, root, "notes/link")
	write(t, root, "notes/link", "regular")
	write(t, root, "notes/run", "new")
	os.Chmod(filepath.Join(root, "notes/run"), 0640)
	save(t, r, root)
	p, err := Plan(ctx, r, root, "", source, []string{"notes"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ops := p.Operations()
	if len(ops) != 2 || ops[0].SourceMode != "120000" || ops[0].Permissions != 0 || ops[1].SourceMode != "100755" || ops[1].Permissions != 0740 {
		t.Fatal(ops)
	}
}
