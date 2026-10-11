package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// restoreProcesses restores n changed files and returns the Git processes the
// restore ran.
func restoreProcesses(t *testing.T, n int) uint64 {
	t.Helper()
	repo, r, _ := fixture(t)
	root := repo.Root
	for i := 0; i < n; i++ {
		write(t, root, fmt.Sprintf("notes/file-%03d", i), "source")
	}
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	for i := 0; i < n; i++ {
		write(t, root, fmt.Sprintf("notes/file-%03d", i), "changed")
	}
	save(t, r, root)
	before := r.SubprocessCount()
	result, err := Execute(ctx, r, root, "", source, []string{"notes"}, true)
	if err != nil || !result.Changed {
		t.Fatalf("restore %d files: %+v %v", n, result, err)
	}
	for i := 0; i < n; i++ {
		if data, err := os.ReadFile(filepath.Join(root, "notes", fmt.Sprintf("file-%03d", i))); err != nil || string(data) != "source" {
			t.Fatalf("restored file %d: %q %v", i, data, err)
		}
	}
	return r.SubprocessCount() - before
}

// Every operation re-checks every path, but only by stat once a path is
// hashed, so the Git processes a restore runs grow linearly with its files.
// Before, each check re-hashed every file: quadrupling the files multiplied
// the processes by about sixteen.
func TestRestoreGitProcessesGrowLinearly(t *testing.T) {
	t.Parallel()
	small, large := restoreProcesses(t, 10), restoreProcesses(t, 40)
	t.Logf("Git processes: 10 files %d, 40 files %d", small, large)
	if large > 6*small {
		t.Fatalf("restoring 4x the files ran %.1fx the Git processes (%d vs %d)", float64(large)/float64(small), large, small)
	}
}
