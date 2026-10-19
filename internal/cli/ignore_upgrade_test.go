package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/exclude"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/testutil"
)

func TestInitRewritesUpgradedIgnoreBlockThatHidesAnExclusion(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	repo.Git("config", "core.ignorecase", "true")
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, home)
	for _, p := range []string{"docs/README.md", "docs/other.txt"} {
		if err := os.MkdirAll(filepath.Join(repo.Root, filepath.Dir(p)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo.Root, p), []byte(p+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	successful(t, repo.Root, "init")
	successful(t, repo.Root, "track", "docs")
	successful(t, repo.Root, "untrack", "docs/README.md")
	// The block 0.1.4 wrote: case-sensitive classes and no staging rule, so
	// /docs/READM[!E]* hides docs/README.md under core.ignorecase.
	selections, err := filepath.Glob(filepath.Join(repo.Root, ".git", "gitlocal", "workspaces", "*", "selection.json"))
	if err != nil || len(selections) != 1 {
		t.Fatal(selections, err)
	}
	data, err := os.ReadFile(selections[0])
	if err != nil {
		t.Fatal(err)
	}
	var state selection.State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	patterns, err := exclude.Compile([]selection.State{state}, false)
	if err != nil {
		t.Fatal(err)
	}
	legacy := exclude.Begin + string(patterns) + exclude.End
	if err := os.WriteFile(filepath.Join(repo.Root, ".git", "info", "exclude"), []byte(legacy+"# user rule\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, diagnostic := callCLI(t, repo.Root, "commit", "-m", "upgraded")
	if code == 0 || !strings.Contains(diagnostic, "git local init") {
		t.Fatalf("commit with the 0.1.4 block (exit %d): %s", code, diagnostic)
	}
	code, out, _ := callCLI(t, repo.Root, "doctor")
	if code == 0 || !strings.Contains(out, "Action: Run git local init") {
		t.Fatalf("doctor with the 0.1.4 block (exit %d):\n%s", code, out)
	}
	if out := successful(t, repo.Root, "init"); !strings.Contains(out, "ignore block") {
		t.Fatalf("init didn't report the rewrite:\n%s", out)
	}
	successful(t, repo.Root, "commit", "-m", "after init")
	after, err := os.ReadFile(filepath.Join(repo.Root, ".git", "info", "exclude"))
	if err != nil || !strings.Contains(string(after), exclude.StagePattern) || !strings.HasSuffix(string(after), "# user rule\n") {
		t.Fatalf("rewritten block: %q %v", after, err)
	}
	if status := string(repo.Git("status", "--porcelain", "--untracked-files=all")); !strings.Contains(status, "docs/README.md") {
		t.Fatalf("excluded file still hidden:\n%s", status)
	}
	if out := successful(t, repo.Root, "init"); strings.Contains(out, "ignore block") {
		t.Fatalf("second init rewrote again:\n%s", out)
	}
}
