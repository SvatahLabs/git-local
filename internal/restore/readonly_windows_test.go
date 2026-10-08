//go:build windows

package restore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRestoreAndRollbackOfReadOnlyFilesOnWindows: a restore replaces a
// read-only file and deletes another; rolling it back recreates both,
// read-only again. On Windows the read-only attribute refuses write handles,
// replacement and deletion.
func TestRestoreAndRollbackOfReadOnlyFilesOnWindows(t *testing.T) {
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "baseline")
	write(t, root, "notes/b", "delete me")
	for _, name := range []string{"notes/a", "notes/b"} {
		if err := os.Chmod(filepath.Join(root, name), 0444); err != nil {
			t.Fatal(err)
		}
	}
	save(t, r, root)
	result, err := execute(ctx, r, root, "", source, []string{"notes"}, true, func(at string) error {
		if at == "before-outcome" {
			return errors.New("stop after every change")
		}
		return nil
	})
	if err == nil || result.TransactionID == "" {
		t.Fatalf("restore didn't stop: %+v %v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "notes/a")); err != nil || string(data) != "source" {
		t.Fatalf("read-only file not replaced: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes/b")); !os.IsNotExist(err) {
		t.Fatalf("read-only file not deleted: %v", err)
	}
	if _, err := Recover(ctx, r, root, result.TransactionID, true); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for name, want := range map[string]string{"notes/a": "baseline", "notes/b": "delete me"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		info, statErr := os.Stat(filepath.Join(root, name))
		if err != nil || statErr != nil || string(data) != want || info.Mode().Perm()&0200 != 0 {
			t.Fatalf("rolled back %s: %q %v %v %v", name, data, info, err, statErr)
		}
	}
	if _, err := Execute(ctx, r, root, "", source, []string{"notes"}, true); err != nil {
		t.Fatalf("restore over read-only files: %v", err)
	}
}
