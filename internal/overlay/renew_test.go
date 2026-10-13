package overlay

import (
	"os"
	"path/filepath"
	"testing"
)

// A path this checkout can't hold as a file (under a symlink or a file, or
// missing) has no cached stat in Git to renew. Renewing failed there, after
// RemoveFilter had removed the filter or with an overlay left "removing", so
// every rerun failed the same way.
func TestRenewTimesSkipsPathsThisCheckoutCantHold(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "x"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	for _, path := range []string{"file/x", "link/x", "missing/x"} {
		if err := renewTimes(root, path); err != nil {
			t.Fatalf("renew times of %s: %v", path, err)
		}
	}
}
