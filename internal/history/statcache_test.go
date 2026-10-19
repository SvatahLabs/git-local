package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
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
	c := &statCache{old: map[string]statEntry{}, next: map[string]statEntry{}, cacheable: func(string) bool { return true }}
	c.store(files, "file", statProbe{info, stamp.Add(time.Millisecond)}, "racy")
	if len(c.next) != 0 {
		t.Fatalf("a racy entry was kept: %v", c.next)
	}
	c.store(files, "file", statProbe{info, stamp.Add(time.Hour)}, "settled")
	if c.next["file"].ID != "settled" {
		t.Fatalf("a settled entry wasn't kept: %v", c.next)
	}
	delete(c.next, "file")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("changed, longer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.store(files, "file", statProbe{info, stamp.Add(time.Hour)}, "stale")
	if len(c.next) != 0 {
		t.Fatalf("an entry for a file that changed during the read was kept: %v", c.next)
	}
}
