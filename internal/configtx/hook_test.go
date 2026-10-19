package configtx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

func TestHookChecksSelectionsFromOtherActiveWorkspaces(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "linked", linked)
	if _, err := workspace.Init(ctx, r, linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(linked, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "notes", "secret"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, r, linked, true, "notes/secret"); err != nil {
		t.Fatal(err)
	}
	conflicts, err := CheckStagedPaths(ctx, r, repo.Root, []string{"app.txt", "notes/secret", "NOTES/SECRET"})
	if err != nil || len(conflicts) != 2 || conflicts[0].Path != "notes/secret" || conflicts[1].Path != "NOTES/SECRET" {
		t.Fatalf("cross-workspace guard: %+v, %v", conflicts, err)
	}
}

func TestGuardCheckTreatsUnselectablePathsByPortableAlias(t *testing.T) {
	s := GuardSnapshot{Selections: []GuardSelection{{WorkspaceID: "w", State: selection.State{Include: []selection.Rule{{Path: "notes/secret", Kind: "file"}, {Path: "private", Kind: "directory"}}}}}}
	unselectable := []string{"aux.c", "con.h", "nul.txt", `a\b.md`, "name.", "\xffbyte.txt", "dir/COM1.log"}
	aliases := []string{`notes\secret`, "notes/secret.", "NOTES/SECRET ", `private\x`, "private./x"}
	conflicts, err := s.Check(append(append([]string{}, unselectable...), aliases...))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != len(aliases) {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	for i, c := range conflicts {
		if c.Path != aliases[i] || c.WorkspaceID != "w" {
			t.Fatalf("conflict %d: %+v", i, c)
		}
	}
	if _, err := s.Check([]string{"bad\x00name"}); err == nil {
		t.Fatal("accepted a NUL in a changed path")
	}
}
