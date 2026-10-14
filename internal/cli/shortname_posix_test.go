//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

// A name like backup~2.txt is an ordinary file outside Windows. Inside a
// tracked directory it must be snapshotted, not break the workspace.
func TestTildeDigitNamesAreOrdinaryOutsideWindows(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, home)
	if err := os.WriteFile(filepath.Join(repo.Root, "notes", "a"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "init")
	successful(t, repo.Root, "track", "notes")
	successful(t, repo.Root, "commit", "-m", "first")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes", "backup~2.txt"), []byte("backup\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "status")
	successful(t, repo.Root, "commit", "-m", "with backup")
	if out := successful(t, repo.Root, "show", "latest:notes/backup~2.txt"); out != "backup\n" {
		t.Fatalf("saved backup~2.txt: %q", out)
	}
	successful(t, repo.Root, "untrack", "notes/backup~2.txt")
}
