package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
)

// Index and HEAD listings grow with the repository. With Git's buffered
// output capped well below their size (16 MiB in production, about 150k
// tracked files), track, commit, status, diff and doctor still work.
func TestLargeIndexAndTreeListingsExceedOutputCap(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	for i := 0; i < 400; i++ {
		path := filepath.Join(repo.Root, "src", fmt.Sprintf("module-%03d", i/20), fmt.Sprintf("file-with-a-long-name-%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("tracked\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	repo.Git("add", "src")
	repo.Git("commit", "-qm", "many tracked files")
	if listing := repo.Git("ls-files", "--stage", "-z"); len(listing) < 32<<10 {
		t.Fatalf("fixture listing is only %d bytes", len(listing))
	}
	saved := gitexec.MaxOutput
	gitexec.MaxOutput = 8 << 10
	defer func() { gitexec.MaxOutput = saved }()
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("local v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	successful(t, repo.Root, "commit", "-m", "local notes")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("local v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out := successful(t, repo.Root, "status"); !strings.Contains(out, "notes.local") {
		t.Fatalf("status:\n%s", out)
	}
	if out := successful(t, repo.Root, "diff"); !strings.Contains(out, "+local v2") {
		t.Fatalf("diff:\n%s", out)
	}
	successful(t, repo.Root, "commit", "-m", "local notes v2")
	if out := successful(t, repo.Root, "show", "latest:notes.local"); out != "local v2\n" {
		t.Fatalf("show: %q", out)
	}
	doctorReportsNoErrors(t, repo.Root)
	// reset --hard asks Git for the filter attribute of every path in the
	// target tree; the response repeats each path, so it exceeds the cap too.
	first := filepath.Join(repo.Root, "src", "module-000", "file-with-a-long-name-0000.txt")
	if err := os.WriteFile(first, []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("commit", "-qam", "change a tracked file")
	successful(t, repo.Root, "reset", "--hard", "HEAD~1")
	if data, err := os.ReadFile(first); err != nil || string(data) != "tracked\n" {
		t.Fatalf("tracked file after reset --hard: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo.Root, "notes.local")); err != nil || string(data) != "local v2\n" {
		t.Fatalf("selected file after reset --hard: %q %v", data, err)
	}
}

// A snapshot tree and its file listing can each exceed the buffered output
// cap; they're listings, so commit, status, log, show, diff, restore and doctor
// still work with the cap enforced (GL-105).
func TestLargeSnapshotListingsExceedOutputCap(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	saved := gitexec.MaxOutput
	gitexec.MaxOutput = 8 << 10
	defer func() { gitexec.MaxOutput = saved }()
	write := func(content string) {
		t.Helper()
		for i := 0; i < 400; i++ {
			path := filepath.Join(repo.Root, "notes", fmt.Sprintf("local-note-with-a-long-name-%04d.txt", i))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("v1\n")
	successful(t, repo.Root, "track", "notes")
	successful(t, repo.Root, "commit", "-m", "many local notes")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes", "local-note-with-a-long-name-0007.txt"), []byte("v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The patch itself is content and stays capped; one changed note fits.
	if out := successful(t, repo.Root, "diff"); !strings.Contains(out, "+v2") {
		t.Fatalf("diff:\n%s", out)
	}
	write("v2\n")
	if out := successful(t, repo.Root, "status"); strings.Count(out, "local-note-with-a-long-name-") != 400 {
		t.Fatalf("status lists %d notes:\n%s", strings.Count(out, "local-note-with-a-long-name-"), out)
	}
	successful(t, repo.Root, "commit", "-m", "many local notes v2")
	if out := successful(t, repo.Root, "log", "--stat"); !strings.Contains(out, "many local notes v2") {
		t.Fatalf("log:\n%s", out)
	}
	if out := successful(t, repo.Root, "show", "latest:notes/local-note-with-a-long-name-0399.txt"); out != "v2\n" {
		t.Fatalf("show: %q", out)
	}
	// Restoring one file still resolves the whole snapshot tree. (Restore time
	// grows with the square of the restored file count: GL-109.)
	note := filepath.Join(repo.Root, "notes", "local-note-with-a-long-name-0399.txt")
	if err := os.Remove(note); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "restore", "--source", "latest", "--", "notes/local-note-with-a-long-name-0399.txt")
	if data, err := os.ReadFile(note); err != nil || string(data) != "v2\n" {
		t.Fatalf("restored note: %q, %v", data, err)
	}
	doctorReportsNoErrors(t, repo.Root)
}
