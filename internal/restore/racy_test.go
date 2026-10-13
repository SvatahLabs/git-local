//go:build darwin || linux

package restore

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/selection"

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
	t.Cleanup(func() { sameIdentity = selection.SameFile })
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
