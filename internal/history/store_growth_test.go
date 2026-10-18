package history

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func snapshotFiles(t *testing.T, g *gitexec.Runner, w *workspace.Context, id string) map[string]fileValue {
	t.Helper()
	ctx := context.Background()
	tree, _, err := readPrevious(ctx, g, w.Store, id, w.RepositoryID, w.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	files, err := readFiles(ctx, g, w.Store, tree)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestBlobIDMatchesGit(t *testing.T) {
	_, g, w := fixture(t)
	large := bytes.Repeat([]byte("0123456789abcdef\x00\r\n"), 70000)
	for _, content := range [][]byte{nil, []byte("a"), []byte("line\r\n\x00binary"), large} {
		want, err := hashRead(context.Background(), g, w.Store, bytes.NewReader(content), false)
		if err != nil {
			t.Fatal(err)
		}
		got, err := blobID(int64(len(content)), bytes.NewReader(content))
		if err != nil || got != want {
			t.Fatalf("blobID of %d bytes = %q, %v; Git gives %q", len(content), got, err, want)
		}
		if got := blobIDString(string(content)); got != want {
			t.Fatalf("blobIDString of %d bytes = %q; Git gives %q", len(content), got, want)
		}
	}
	if _, err := blobID(5, strings.NewReader("abc")); err == nil {
		t.Fatal("a short read was hashed")
	}
}

// status and an unchanged full snapshot start the same number of Git
// processes whatever the number of selected files (GL-135).
func TestStatusAndSnapshotGitProcessesDontGrowWithFiles(t *testing.T) {
	measure := func(n int) (status, snapshot uint64) {
		r, g, _ := fixture(t)
		for i := 0; i < n; i++ {
			write(t, r.Root, fmt.Sprintf(".local/d%d/f%03d", i%4, i), []byte(fmt.Sprintf("file %d\n", i)))
		}
		if err := os.Symlink("f000", filepath.Join(r.Root, ".local/link")); err != nil {
			t.Fatal(err)
		}
		change(t, g, r.Root, true, ".local")
		save(t, g, r.Root)
		write(t, r.Root, ".local/d1/f001", []byte("edited\n"))
		start := g.SubprocessCount()
		result, err := Status(context.Background(), g, r.Root)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Files) != 1 || result.Files[0] != (FileChange{"M", ".local/d1/f001"}) {
			t.Fatalf("status = %+v", result.Files)
		}
		status = g.SubprocessCount() - start
		saved := save(t, g, r.Root)
		if !saved.Changed {
			t.Fatal("edit not saved")
		}
		again, err := Status(context.Background(), g, r.Root)
		if err != nil || len(again.Files) != 0 {
			t.Fatalf("status after save = %+v, %v", again.Files, err)
		}
		return status, saved.Subprocesses
	}
	smallStatus, smallSnapshot := measure(20)
	largeStatus, largeSnapshot := measure(80)
	t.Logf("status %d and %d, snapshot %d and %d Git processes", smallStatus, largeStatus, smallSnapshot, largeSnapshot)
	if largeStatus != smallStatus {
		t.Fatalf("status started %d Git processes for 20 files and %d for 80", smallStatus, largeStatus)
	}
	if largeSnapshot > smallSnapshot+2 {
		t.Fatalf("a one-file snapshot started %d Git processes for 20 files and %d for 80", smallSnapshot, largeSnapshot)
	}
}

// A file whose bytes match a blob of the previous snapshot under another
// path or mode is recorded with that blob, and a new one is written.
func TestSnapshotReusesKnownBlobsAndWritesNewOnes(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, ".local/a", []byte("same\n"))
	change(t, g, r.Root, true, ".local")
	first := save(t, g, r.Root)
	write(t, r.Root, ".local/b", []byte("same\n"))
	write(t, r.Root, ".local/c", []byte("new\n"))
	second := save(t, g, r.Root)
	files := snapshotFiles(t, g, w, second.ID)
	if files[".local/a"] != snapshotFiles(t, g, w, first.ID)[".local/a"] || files[".local/b"].ID != files[".local/a"].ID {
		t.Fatalf("files = %+v", files)
	}
	content := gitHistory(t, g, w, "cat-file", "blob", files[".local/c"].ID)
	if string(content) != "new\n" {
		t.Fatalf("new blob holds %q", content)
	}
}

// A scoped snapshot after untracking a file inside a tracked directory drops
// that file instead of refusing; earlier snapshots keep it (GL-137).
func TestScopedSnapshotDropsUntrackedCarriedForwardPath(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, ".local/a", []byte("a\n"))
	write(t, r.Root, ".local/sub/b", []byte("b\n"))
	write(t, r.Root, ".local/c", []byte("c\n"))
	change(t, g, r.Root, true, ".local")
	first := save(t, g, r.Root)
	change(t, g, r.Root, false, ".local/sub/b")
	write(t, r.Root, ".local/a", []byte("a2\n"))
	write(t, r.Root, ".local/c", []byte("c2\n"))
	result, err := CommitScoped(context.Background(), g, r.Root, "scoped", "", []string{".local/a"})
	if err != nil {
		t.Fatal(err)
	}
	files := snapshotFiles(t, g, w, result.ID)
	if _, ok := files[".local/sub/b"]; ok {
		t.Fatalf("untracked path carried forward: %+v", files)
	}
	before := snapshotFiles(t, g, w, first.ID)
	if files[".local/c"] != before[".local/c"] || files[".local/a"] == before[".local/a"] {
		t.Fatalf("scoped snapshot files = %+v, before %+v", files, before)
	}
	if _, ok := before[".local/sub/b"]; !ok {
		t.Fatal("earlier snapshot lost the untracked path")
	}
	_, metadata, err := readPrevious(context.Background(), g, w.Store, result.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(metadata.ChangedPaths, ",") != ".local/a,.local/sub/b" {
		t.Fatalf("changed paths = %q", metadata.ChangedPaths)
	}
}

// A snapshot packs the store once loose objects pass the limit, deleting
// objects no ref reaches, and leaves a store that passes the full check
// (GL-136).
func TestSnapshotPacksAGrownStore(t *testing.T) {
	r, g, w := fixture(t)
	defer func(objects int64) { autoPackObjects = objects }(autoPackObjects)
	autoPackObjects = 1 << 40
	for i := 0; i < 30; i++ {
		write(t, r.Root, fmt.Sprintf(".local/f%02d", i), []byte(fmt.Sprintf("file %d\n", i)))
	}
	change(t, g, r.Root, true, ".local")
	first := save(t, g, r.Root)
	stray, err := hashRead(context.Background(), g, w.Store, strings.NewReader("left by an interrupted snapshot\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	count, _, err := gitexec.LooseObjects(w.Store)
	if err != nil || count < 30 {
		t.Fatalf("loose objects = %d, %v", count, err)
	}
	autoPackObjects = 20
	write(t, r.Root, ".local/f00", []byte("edited\n"))
	second := save(t, g, r.Root)
	if len(second.Warnings) != 0 {
		t.Fatalf("warnings = %q", second.Warnings)
	}
	if count, _, err = gitexec.LooseObjects(w.Store); err != nil || count != 0 {
		t.Fatalf("loose objects after the snapshot = %d, %v", count, err)
	}
	if _, err := g.History(context.Background(), w.Store, nil, "cat-file", "-e", stray); err == nil {
		t.Fatal("an unreferenced object survived packing")
	}
	if err := g.CheckStore(context.Background(), w.Store); err != nil {
		t.Fatal(err)
	}
	if files := snapshotFiles(t, g, w, first.ID); len(files) != 30 {
		t.Fatalf("first snapshot has %d files", len(files))
	}
	// Below the limit nothing is packed.
	write(t, r.Root, ".local/f01", []byte("edited\n"))
	save(t, g, r.Root)
	if count, _, err = gitexec.LooseObjects(w.Store); err != nil || count == 0 {
		t.Fatalf("loose objects below the limit = %d, %v", count, err)
	}
}

// When another command holds the lock, the snapshot is saved and packing is
// left to a later snapshot without a warning. Not parallel: it replaces the
// limits.
func TestSnapshotLeavesPackingWhenTheLockIsHeld(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, ".local/a", []byte("a\n"))
	change(t, g, r.Root, true, ".local")
	save(t, g, r.Root)
	defer func(objects int64, wait time.Duration) { autoPackObjects, autoPackLockWait = objects, wait }(autoPackObjects, autoPackLockWait)
	autoPackObjects, autoPackLockWait = 1, 200*time.Millisecond
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	lock, err := common.LockExisting(context.Background(), "gitlocal/locks/repository.lock")
	if err != nil {
		t.Fatal(err)
	}
	result := Result{Changed: true, store: w.Store}
	autoMaintain(context.Background(), g, r.Root, &result)
	lock.Close()
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %q", result.Warnings)
	}
	if count, _, err := gitexec.LooseObjects(w.Store); err != nil || count == 0 {
		t.Fatalf("loose objects = %d, %v", count, err)
	}
}

// Tree IDs computed in process match Git's, including names that sort
// differently as trees ("a" sorts after "a.b" and "a-c").
func TestTreeIDsMatchGit(t *testing.T) {
	_, g, w := fixture(t)
	ctx := context.Background()
	blob, err := hashRead(ctx, g, w.Store, strings.NewReader("x\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	root := &directory{}
	for _, path := range []string{"a/b", "a.b", "a-c", "a/d/e", "z", "a0", "é/ü"} {
		root.add(path, "100644", blob)
	}
	root.add("link", "120000", blob)
	root.add("run", "100755", blob)
	id, err := root.write(ctx, g, w.Store)
	if err != nil {
		t.Fatal(err)
	}
	listing := gitHistory(t, g, w, "ls-tree", "-r", "-z", "--full-tree", id)
	if n := strings.Count(string(listing), "\x00"); n != 9 {
		t.Fatalf("tree lists %d files", n)
	}
	// Known trees are not written again.
	start := g.SubprocessCount()
	if again, err := root.writeExcept(ctx, g, w.Store, id); err != nil || again != id || g.SubprocessCount() != start {
		t.Fatalf("known tree = %s, %v, %d processes", again, err, g.SubprocessCount()-start)
	}
	if empty, err := (&directory{}).write(ctx, g, w.Store); err != nil || empty != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Fatalf("empty tree = %s, %v", empty, err)
	}
}
