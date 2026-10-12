package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// restoreWork restores n changed files and returns the path checks the
// restore's guard made and the journal bytes it wrote.
func restoreWork(t *testing.T, n int) (int, int) {
	t.Helper()
	checks, written := 0, 0
	pathChecked = func(string) { checks++ }
	journalWritten = func(n int) { written += n }
	defer func() { pathChecked, journalWritten = func(string) {}, func(int) {} }()
	restoreProcesses(t, n)
	return checks, written
}

// Each operation checks its own path and the cheap global state; every path
// is checked again only at phase boundaries, at the end, and when HEAD, the
// branch or the index file changed. The journal, which holds every operation,
// records progress at powers of two. Before, every operation checked every
// path and rewrote the journal, so quadrupling the files multiplied both by
// about sixteen. Not parallel: it replaces pathChecked and journalWritten.
func TestRestorePathChecksGrowLinearly(t *testing.T) {
	smallChecks, smallBytes := restoreWork(t, 10)
	largeChecks, largeBytes := restoreWork(t, 40)
	t.Logf("path checks: 10 files %d, 40 files %d; journal bytes: %d, %d", smallChecks, largeChecks, smallBytes, largeBytes)
	if smallChecks == 0 || largeChecks > 6*smallChecks {
		t.Fatalf("restoring 4x the files made %.1fx the path checks (%d vs %d)", float64(largeChecks)/float64(smallChecks), largeChecks, smallChecks)
	}
	if smallBytes == 0 || largeBytes > 6*smallBytes {
		t.Fatalf("restoring 4x the files wrote %.1fx the journal bytes (%d vs %d)", float64(largeBytes)/float64(smallBytes), largeBytes, smallBytes)
	}
}

// Between full checks only the operation's own path is checked, so a change
// elsewhere must still stop the restore before anything it touches is
// overwritten: a pending file's edit at its own operation, an installed file's
// edit at the final check, and new ownership at the next operation through the
// changed index. A rewritten index with the same entries is only re-checked.
func TestRestoreDetectsChangesBetweenFullChecks(t *testing.T) {
	for _, kind := range []string{"pending-edit", "installed-edit", "owned", "index-rewritten"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			repo, r, w := fixture(t)
			root := repo.Root
			for _, name := range []string{"a", "b", "c"} {
				write(t, root, "notes/"+name, "source")
			}
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			for _, name := range []string{"a", "b", "c"} {
				write(t, root, "notes/"+name, "baseline")
			}
			save(t, r, root)
			_, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
				if at != "progress:0" {
					return nil
				}
				switch kind {
				case "pending-edit":
					write(t, root, "notes/c", "external")
				case "installed-edit":
					write(t, root, "notes/a", "external")
				case "owned":
					repo.Git("add", "-f", "notes/c")
				case "index-rewritten":
					index := filepath.Join(w.Admin, "index")
					b, e := os.ReadFile(index)
					if e != nil {
						return e
					}
					if e = os.WriteFile(index+".new", b, 0644); e != nil {
						return e
					}
					return os.Rename(index+".new", index)
				}
				return nil
			})
			want := map[string]string{"a": "source", "b": "source", "c": "source"}
			switch kind {
			case "pending-edit":
				want = map[string]string{"a": "source", "c": "external"}
			case "installed-edit":
				want = map[string]string{"a": "external"}
			case "owned":
				want["b"], want["c"] = "baseline", "baseline"
			}
			if kind == "index-rewritten" {
				if err != nil {
					t.Fatalf("restore after an unchanged index rewrite: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "restore transaction") {
				t.Fatalf("restore accepted a change between full checks: %v", err)
			}
			for name, value := range want {
				if b, e := os.ReadFile(filepath.Join(root, "notes", name)); e != nil || string(b) != value {
					t.Fatalf("notes/%s: %q %v, want %q", name, b, e, value)
				}
			}
		})
	}
}
