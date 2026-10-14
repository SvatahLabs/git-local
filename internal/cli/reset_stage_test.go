package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestResetHardNamesCommittedStagingFile(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	successful(t, repo.Root, "init")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	stage := ".gitlocal-tmp-0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69"
	if err := os.WriteFile(filepath.Join(repo.Root, stage), []byte("decrypted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "-f", stage)
	repo.Git("commit", "-qm", "stage leak")
	leaked := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	repo.Git("rm", "-q", "--cached", stage)
	repo.Git("commit", "-qm", "remove stage")
	code, _, diagnostic := callCLI(t, repo.Root, "reset", "--hard", leaked)
	if code != 3 || !strings.Contains(diagnostic, "staging file") || !strings.Contains(diagnostic, stage) || strings.Contains(diagnostic, "from workspace") {
		t.Fatalf("reset message: %d %s", code, diagnostic)
	}
}
