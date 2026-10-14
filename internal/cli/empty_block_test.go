package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/exclude"
	"gitlocal/internal/testutil"
)

// TestInitWritesStageRuleWithoutSelections: 0.1.4 wrote no managed block for
// an empty selection, so such a repository has no .gitlocal-tmp-* rule. Init
// writes the current block whether or not anything is selected, and doctor
// flags a missing one.
func TestInitWritesStageRuleWithoutSelections(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	path := filepath.Join(repo.Root, ".git", "info", "exclude")
	hasStageRule := func() bool {
		data, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(data), "\n"+exclude.StagePattern+"\n") && strings.Contains(string(data), exclude.Begin)
	}
	if !hasStageRule() {
		t.Fatal("a fresh init wrote no managed block")
	}
	// The state 0.1.4 left: user rules and no block.
	if err := os.WriteFile(path, []byte("*.log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := callCLI(t, repo.Root, "doctor")
	if code != 0 || !strings.Contains(out, "managed ignore block is missing") || !strings.Contains(out, "git local init") {
		t.Fatalf("doctor with no managed block (exit %d):\n%s", code, out)
	}
	if out := successful(t, repo.Root, "init"); !strings.Contains(out, "Managed ignore block: added.") {
		t.Fatalf("init output:\n%s", out)
	}
	if !hasStageRule() {
		t.Fatal("init didn't write the managed block")
	}
	if data, _ := os.ReadFile(path); !strings.HasSuffix(string(data), "*.log\n") {
		t.Fatalf("init dropped the user's rules:\n%s", data)
	}
	if out := successful(t, repo.Root, "doctor"); strings.Contains(out, "managed ignore block is missing") {
		t.Fatalf("doctor after init:\n%s", out)
	}
	// Without info/exclude at all, as in a repository created without Git's
	// template.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "init")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 || !hasStageRule() {
		t.Fatalf("init without info/exclude: %v %v", info, err)
	}
}

// TestFreshInitSaysBlockAdded: a fresh repository has no managed block, so
// init adds one; "rewritten for this release" is only for an older block.
func TestFreshInitSaysBlockAdded(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, home)
	if out := successful(t, repo.Root, "init"); !strings.Contains(out, "Managed ignore block: added.") || strings.Contains(out, "rewritten") {
		t.Fatalf("fresh init output:\n%s", out)
	}
	if out := successful(t, repo.Root, "init"); strings.Contains(out, "Managed ignore block") {
		t.Fatalf("second init output:\n%s", out)
	}
}
