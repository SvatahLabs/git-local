package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// waitUntilSettled waits until no file under dir has a racy timestamp, so a
// stat cache may keep what status hashes from them.
func waitUntilSettled(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		racy := false
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := os.Lstat(path)
			if err == nil && selection.Racy(info, time.Now()) {
				racy = true
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if !racy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timestamps stay racy")
		}
		time.Sleep(selection.RacyMargin)
	}
}

// GL-138: a warm status and a snapshot without changes read no selected
// file; a same-size edit with the mtime put back, made within the
// timestamps' tick, is still seen by both.
func TestWarmStatusAndSnapshotReadNoSelectedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no stat cache")
	}
	r, g, _ := fixture(t)
	r.Commit()
	const files = 20
	for i := range files {
		write(t, r.Root, fmt.Sprintf("notes/%02d.txt", i), []byte(fmt.Sprintf("note %02d\n", i)))
	}
	change(t, g, r.Root, true, "notes")
	save(t, g, r.Root)
	waitUntilSettled(t, filepath.Join(r.Root, "notes"))
	status := func() ([]FileChange, int64) {
		t.Helper()
		before := filesHashed.Load()
		result, err := Status(context.Background(), g, r.Root)
		if err != nil {
			t.Fatal(err)
		}
		return result.Files, filesHashed.Load() - before
	}
	if changes, read := status(); len(changes) != 0 || read > files {
		t.Fatalf("first status: changes %v, %d files read", changes, read)
	}
	if changes, read := status(); len(changes) != 0 || read != 0 {
		t.Fatalf("warm status: changes %v, %d files read, want 0", changes, read)
	}
	before := filesHashed.Load()
	if result := save(t, g, r.Root); result.Changed || filesHashed.Load() != before {
		t.Fatalf("snapshot without changes: changed %v, %d files read, want 0", result.Changed, filesHashed.Load()-before)
	}

	edited := filepath.Join(r.Root, "notes", "07.txt")
	info, err := os.Lstat(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edited, []byte("NOTE 07\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(edited, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	want := []FileChange{{"M", "notes/07.txt"}}
	if changes, read := status(); !reflect.DeepEqual(changes, want) || read != 1 {
		t.Fatalf("status after a same-size edit: changes %v, %d files read, want 1", changes, read)
	}
	before = filesHashed.Load()
	if result := save(t, g, r.Root); !result.Changed || filesHashed.Load()-before != 1 {
		t.Fatalf("snapshot after a same-size edit: changed %v, %d files read, want 1", result.Changed, filesHashed.Load()-before)
	}
	if changes, _ := status(); len(changes) != 0 {
		t.Fatalf("status after the snapshot: %v", changes)
	}

	// The cache is private state that init in another worktree accepts.
	if _, err := os.Stat(filepath.Join(r.Root, ".git", "gitlocal", StatCacheDir)); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	r.Git("worktree", "add", "-qb", "linked", linked)
	if _, err := workspace.Init(context.Background(), g, linked); err != nil {
		t.Fatal(err)
	}
}

// GL-138: the cache keeps no blob ID hashed from a file whose timestamps
// fall within the margin before the read, nor one that changed meanwhile.
func TestStatCacheRefusesRacyEntries(t *testing.T) {
	if !statKeysSupported {
		t.Skip("no stat cache on this platform")
	}
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := storage.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	info, err := files.Lstat("file")
	if err != nil {
		t.Fatal(err)
	}
	stamp := info.ModTime()
	if ct := selection.ChangeTime(info); ct.After(stamp) {
		stamp = ct
	}
	read, err := files.Observe("file")
	if err != nil {
		t.Fatal(err)
	}
	c := &statCache{old: map[string]statEntry{}, next: map[string]statEntry{}, cacheable: func(string) bool { return true }}
	c.store("file", statProbe{info, stamp.Add(time.Millisecond)}, read, "racy")
	if len(c.next) != 0 {
		t.Fatalf("a racy entry was kept: %v", c.next)
	}
	c.store("file", statProbe{info, stamp.Add(time.Hour)}, read, "settled")
	if c.next["file"].ID != "settled" {
		t.Fatalf("a settled entry wasn't kept: %v", c.next)
	}
	delete(c.next, "file")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("changed, longer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if read, err = files.Observe("file"); err != nil {
		t.Fatal(err)
	}
	c.store("file", statProbe{info, stamp.Add(time.Hour)}, read, "stale")
	if len(c.next) != 0 {
		t.Fatalf("an entry for a file that changed during the read was kept: %v", c.next)
	}
}

// statCacheFixture writes path with content in a new directory and returns
// the directory, opened, and the file's Lstat.
func statCacheFixture(t *testing.T, path, content string) (string, *storage.Dir, os.FileInfo) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := storage.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	info, err := files.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return dir, files, info
}

// writeLike writes content to name with the mtime of like, as rsync -a or
// cp -p into a temporary file would before renaming it over the original.
func writeLike(t *testing.T, name, content string, like os.FileInfo) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, like.ModTime(), like.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// GL-138 review F1: a file renamed over the cached one between lookup's
// Lstat and its open, with the same size and mtime, isn't taken for it.
func TestStatCacheLookupChecksTheOpenedFile(t *testing.T) {
	if !statKeysSupported {
		t.Skip("no stat cache on this platform")
	}
	dir, files, info := statCacheFixture(t, "file", "content A\n")
	key, ok := statKeyOf(info)
	if !ok {
		t.Fatal("no stat key")
	}
	c := &statCache{old: map[string]statEntry{"file": {key, "id-A"}}, next: map[string]statEntry{}, cacheable: func(string) bool { return true }}
	if id, _, _, cached := c.lookup(files, "file", info.Mode()); !cached || id != "id-A" {
		t.Fatalf("an unchanged file missed the cache: %q %v", id, cached)
	}
	writeLike(t, filepath.Join(dir, "replacement"), "content B\n", info)
	statCacheBeforeObserve = func(path string) {
		if err := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, path)); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { statCacheBeforeObserve = nil })
	c.next = map[string]statEntry{}
	if id, _, probe, cached := c.lookup(files, "file", info.Mode()); cached || probe.info == nil {
		t.Fatalf("a file renamed over the cached one was taken for it: %q", id)
	}
	if _, ok := c.next["file"]; ok {
		t.Fatal("the stale entry was kept")
	}
}

// GL-138 review F2: store keeps an ID only for the file that was read, even
// when the path names the probed file again by the time it is checked.
func TestStatCacheStoreChecksTheFileRead(t *testing.T) {
	if !statKeysSupported {
		t.Skip("no stat cache on this platform")
	}
	t.Parallel()
	dir, files, info := statCacheFixture(t, "file", "content A\n")
	hashed := info.ModTime().Add(time.Hour)
	writeLike(t, filepath.Join(dir, "other"), "content B\n", info)
	other, err := files.Observe("other")
	if err != nil {
		t.Fatal(err)
	}
	c := &statCache{old: map[string]statEntry{}, next: map[string]statEntry{}, cacheable: func(string) bool { return true }}
	c.store("file", statProbe{info, hashed}, other, "id-B")
	if len(c.next) != 0 {
		t.Fatalf("the probed file's key was stored with another file's ID: %v", c.next)
	}
	same, err := files.Observe("file")
	if err != nil {
		t.Fatal(err)
	}
	c.store("file", statProbe{info, hashed}, same, "id-A")
	if c.next["file"].ID != "id-A" {
		t.Fatalf("the file read wasn't stored: %v", c.next)
	}
}

// statCacheWorkspace is a workspace whose private state directory is a new
// temporary directory.
func statCacheWorkspace(t *testing.T) (workspace.Context, *storage.Dir) {
	t.Helper()
	state := t.TempDir()
	dir, err := storage.OpenDir(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	return workspace.Context{Store: filepath.Join(state, "history.git"), WorkspaceID: "0b7c6a8e-4f53-4d0e-9a51-0c2b7f6a1d3e"}, dir
}

func readStatCache(t *testing.T, w workspace.Context) (statCacheFile, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(w.Store), StatCacheDir, w.WorkspaceID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var file statCacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file, data
}

// GL-138 review F3: a path that has become sensitive, or is no longer
// selected, keeps no entry: not when the cache is read, and not through a
// scoped snapshot that leaves it out of scope.
func TestStatCacheDropsEntriesItMayNotKeep(t *testing.T) {
	if !statKeysSupported {
		t.Skip("no stat cache on this platform")
	}
	t.Parallel()
	w, _ := statCacheWorkspace(t)
	all := func(string) bool { return true }
	c := loadStatCache(w, all)
	for _, path := range []string{"plain", "secret", "gone"} {
		c.next[path] = statEntry{statKey{Ino: 1, Size: 1}, "id-" + path}
	}
	c.save()
	c.close()

	c = loadStatCache(w, func(path string) bool { return path != "secret" })
	defer c.close()
	if _, ok := c.old["secret"]; ok {
		t.Fatal("a sensitive path's entry was loaded")
	}
	// A scoped snapshot of nothing: every path is out of scope, and "gone"
	// is no longer selected.
	c.retain(func(path string) bool { return path != "gone" })
	c.save()
	file, data := readStatCache(t, w)
	if want := map[string]statEntry{"plain": {statKey{Ino: 1, Size: 1}, "id-plain"}}; !reflect.DeepEqual(file.Entries, want) {
		t.Fatalf("entries after a scoped run: %s", data)
	}

	// Loading alone, with nothing else changed, also rewrites the file
	// without the newly sensitive path.
	c2 := loadStatCache(w, func(path string) bool { return path != "plain" })
	defer c2.close()
	c2.save()
	if file, data := readStatCache(t, w); len(file.Entries) != 0 {
		t.Fatalf("a newly sensitive path's entry was left in the file: %s", data)
	}
}

// GL-138 review F5: save doesn't write a cache that load would ignore, and
// removes the old one so later runs don't read it either.
func TestStatCacheSaveSkipsOversizeCache(t *testing.T) {
	if !statKeysSupported {
		t.Skip("no stat cache on this platform")
	}
	w, _ := statCacheWorkspace(t)
	c := loadStatCache(w, func(string) bool { return true })
	c.next["small"] = statEntry{statKey{Ino: 1}, "id"}
	c.save()
	c.close()
	_, data := readStatCache(t, w)
	limit := statCacheLimitBytes
	statCacheLimitBytes = int64(len(data)) + 10
	t.Cleanup(func() { statCacheLimitBytes = limit })

	c = loadStatCache(w, func(string) bool { return true })
	defer c.close()
	if len(c.old) != 1 {
		t.Fatalf("the cache under the limit wasn't read: %v", c.old)
	}
	for i := range 10 {
		c.next[fmt.Sprintf("large-%02d", i)] = statEntry{statKey{Ino: uint64(i)}, "id"}
	}
	c.save()
	name := filepath.Join(filepath.Dir(w.Store), StatCacheDir, w.WorkspaceID+".json")
	if info, err := os.Stat(name); !os.IsNotExist(err) {
		size := int64(-1)
		if info != nil {
			size = info.Size()
		}
		t.Fatalf("a cache over the %d-byte limit was left (%d bytes): %v", statCacheLimitBytes, size, err)
	}
}

// DropStatCaches removes every workspace's cache, and accepts none.
func TestDropStatCaches(t *testing.T) {
	t.Parallel()
	_, state := statCacheWorkspace(t)
	if err := DropStatCaches(state); err != nil {
		t.Fatal(err)
	}
	if err := state.MkdirAll(StatCacheDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json", ".gitlocal-tmp-x"} {
		if err := state.AtomicWrite(filepath.Join(StatCacheDir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := DropStatCaches(state); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Lstat(StatCacheDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat caches remain: %v", err)
	}
}
