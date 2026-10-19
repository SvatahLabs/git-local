package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/exclude"
)

// TestInitRewritesBlockDespiteUnrelatedSelectionProblem: an upgraded
// repository has a block from an earlier release and, separately, a
// selected path that Git now tracks. Init must still rewrite the block, which
// is the remedy doctor names, and name the real problem.
func TestInitRewritesBlockDespiteUnrelatedSelectionProblem(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	path := filepath.Join(repo.Root, ".git", "info", "exclude")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(data), exclude.Begin+exclude.StagePattern+"\n", exclude.Begin, 1)
	if legacy == string(data) {
		t.Fatalf("no current block to downgrade:\n%s", data)
	}
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "-f", "notes.local")
	repo.Git("commit", "-qm", "track a selected file by mistake")
	code, out, diagnostic := callCLI(t, repo.Root, "init")
	if code != 0 || !strings.Contains(out, "Managed ignore block: rewritten") || !strings.Contains(diagnostic, "Warning:") || !strings.Contains(diagnostic, "notes.local") {
		t.Fatalf("init: exit %d\n%s%s", code, out, diagnostic)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), exclude.Begin+exclude.StagePattern+"\n") {
		t.Fatalf("block not rewritten: %v\n%s", err, data)
	}
}
