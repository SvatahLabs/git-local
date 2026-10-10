package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
)

// Index and HEAD listings grow with the repository. With Git's buffered
// output capped well below their size (16 MiB in production, about 150k
// tracked files), track, commit, status, diff and doctor still work.
func TestLargeIndexAndTreeListingsExceedOutputCap(t *testing.T) {
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
}
