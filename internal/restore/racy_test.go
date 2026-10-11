//go:build darwin || linux

package restore

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// On a filesystem with coarse timestamps, a same-size rewrite in place can
// leave every Lstat field equal. The destination is hashed again just before
// the rename, so such an edit made while the stage was written is preserved.
// Not parallel: it replaces sameIdentity.
func TestPreRenameCheckHashesDestinationAgain(t *testing.T) {
	sameIdentity = func(a, b fs.FileInfo) bool {
		sa, sb := a.Sys().(*unix.Stat_t), b.Sys().(*unix.Stat_t)
		return sa.Dev == sb.Dev && sa.Ino == sb.Ino
	}
	t.Cleanup(func() { sameIdentity = sameFile })
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "baseline")
	save(t, r, root)
	path := filepath.Join(root, "notes", "a")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
		if at != "before-install:0" {
			return nil
		}
		// Same size, same inode, modification time put back.
		if err := os.WriteFile(path, []byte("external"), 0644); err != nil {
			return err
		}
		return os.Chtimes(path, info.ModTime(), info.ModTime())
	})
	if err == nil {
		t.Fatal("restore renamed over an edit made while staging")
	}
	if b, e := os.ReadFile(path); e != nil || string(b) != "external" {
		t.Fatalf("external edit overwritten: %q %v", b, e)
	}
}

func TestRacyTimestamps(t *testing.T) {
	hashed := time.Date(2026, 10, 11, 12, 0, 10, 500, time.UTC)
	for _, c := range []struct {
		mtime time.Time
		racy  bool
	}{
		{hashed.Add(-time.Millisecond), false},
		{hashed, true},
		{hashed.Add(time.Second), true},
		{time.Date(2026, 10, 11, 12, 0, 9, 0, time.UTC), true},  // whole second within 2 s
		{time.Date(2026, 10, 11, 12, 0, 8, 0, time.UTC), false}, // whole second, 2 s before
	} {
		if got := racy(fakeInfo{c.mtime}, hashed); got != c.racy {
			t.Errorf("mtime %v: racy %v, want %v", c.mtime, got, c.racy)
		}
	}
}

type fakeInfo struct{ mtime time.Time }

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return 0644 }
func (f fakeInfo) ModTime() time.Time { return f.mtime }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }
