package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// TestGuardRefusesHookRunForAnotherRepository: with git --git-dir=A
// --work-tree=B, Git runs A's hooks in B. The guard finds its repository from
// the working directory, so it must refuse rather than pass because B has no
// git-local state.
func TestGuardRefusesHookRunForAnotherRepository(t *testing.T) {
	a := testutil.New(t)
	a.Commit()
	home := filepath.Join(a.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, a.Root, "init")
	if err := os.WriteFile(filepath.Join(a.Root, "notes.local"), []byte("local only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, a.Root, "track", "notes.local")
	a.Git("add", "-f", "notes.local")
	b := testutil.New(t)
	b.Commit()

	t.Setenv("GIT_DIR", filepath.Join(a.Root, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(a.Root, ".git", "index"))
	for _, args := range [][]string{{"guard", "staged"}, {"guard", "push", "origin"}} {
		code, _, diagnostic := callCLI(t, b.Root, args...)
		if code != 3 || !strings.Contains(diagnostic, filepath.Join(a.Root, ".git")) || !strings.Contains(diagnostic, b.Root) {
			t.Fatalf("%v in another repository's worktree: exit %d: %s", args, code, diagnostic)
		}
	}
	// The same GIT_DIR from its own worktree, absolute or relative, is
	// checked as usual and finds the staged selected file.
	for _, gitDir := range []string{filepath.Join(a.Root, ".git"), ".git"} {
		t.Setenv("GIT_DIR", gitDir)
		code, _, diagnostic := callCLI(t, a.Root, "guard", "staged")
		if code != 1 || !strings.Contains(diagnostic, "notes.local") {
			t.Fatalf("GIT_DIR=%s: exit %d: %s", gitDir, code, diagnostic)
		}
	}
}
