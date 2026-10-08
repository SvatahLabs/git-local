//go:build windows

package storage

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestReadOnlyStagesOnWindows: mode 0444 sets the read-only attribute, which
// refuses handles opened for writing, replacement by MoveFileEx and deletion.
// Staging, resuming, publishing and deleting read-only files must still work.
func TestReadOnlyStagesOnWindows(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	write := func(content string) func(io.Writer) error {
		return func(w io.Writer) error { _, err := io.WriteString(w, content); return err }
	}
	if err := d.StageNamed(".gitlocal-tmp-a", 0444, write("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := d.SyncStaged(".gitlocal-tmp-a"); err != nil {
		t.Fatalf("sync of a read-only stage flushed in this process: %v", err)
	}
	// A stage a dead process left: not flushed in this process.
	flushedStages.Delete(filepath.Join(d.name, ".gitlocal-tmp-a"))
	if err := d.SyncStaged(".gitlocal-tmp-a"); err != nil {
		t.Fatalf("sync of a read-only stage from another process: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, ".gitlocal-tmp-a")); err != nil || info.Mode().Perm()&0200 != 0 {
		t.Fatalf("sync dropped the read-only attribute: %v %v", info, err)
	}
	if err := d.ChmodStaged(".gitlocal-tmp-a", 0444); err != nil {
		t.Fatalf("chmod of a read-only stage: %v", err)
	}
	// Publish over a read-only target.
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "target"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := d.Rename(".gitlocal-tmp-a", "target"); err != nil {
		t.Fatalf("replace a read-only target: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "target")); err != nil || string(data) != "first\n" {
		t.Fatalf("target: %q %v", data, err)
	}
	if err := d.Remove("target"); err != nil {
		t.Fatalf("delete a read-only file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "target")); !os.IsNotExist(err) {
		t.Fatalf("read-only file not deleted: %v", err)
	}
}
