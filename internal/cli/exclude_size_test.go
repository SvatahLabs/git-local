package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each excluded name inside a tracked directory costs about L*L+21*L bytes of
// managed patterns once names differ near their start. Thirty such
// sixty-character names exceeded the old 128 KiB cap.
func TestUntrackThirtyLongNamesInOneDirectory(t *testing.T) {
	repo := layoutRepo(t)
	names := make([]string, 30)
	for i := range names {
		names[i] = filepath.Join("data", fmt.Sprintf("%03d-%s", i, strings.Repeat("excluded-name-", 4)))
		if len(filepath.Base(names[i])) != 60 {
			t.Fatalf("name length %d", len(filepath.Base(names[i])))
		}
		if err := os.MkdirAll(filepath.Join(repo.Root, "data"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo.Root, names[i]), []byte("shared\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "data", "local.txt"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "data")
	successful(t, repo.Root, append([]string{"untrack"}, names...)...)
	ignored := func(path string) bool {
		cmd := exec.Command(repo.GitPath, "check-ignore", "-q", "--no-index", path)
		cmd.Dir, cmd.Env = repo.Root, repo.Env
		return cmd.Run() == nil
	}
	if !ignored("data/local.txt") || !ignored("data/new-local-file") {
		t.Fatal("selected files in data/ aren't ignored")
	}
	for _, name := range names {
		if ignored(name) {
			t.Fatalf("excluded %s is still ignored", name)
		}
	}
	successful(t, repo.Root, "commit", "-m", "local data")
	if out := successful(t, repo.Root, "show", "latest:data/local.txt"); out != "local\n" {
		t.Fatalf("show: %q", out)
	}
}
