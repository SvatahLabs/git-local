package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
)

// skippedRemoveFixture gives a linked worktree seeded with notes/Secret.txt
// under a policy from an earlier release that spells the rule differently, so
// snapshots that would save that file are refused.
func skippedRemoveFixture(t *testing.T) (*testutil.Repo, string, func(root, path, content string)) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, home)
	write := func(root, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(repo.Root, "notes/Secret.txt", "TOKEN=v1\n")
	write(repo.Root, "notes/plain.txt", "plain v1\n")
	successful(t, repo.Root, "init")
	successful(t, repo.Root, "track", "notes")
	successful(t, repo.Root, "commit", "-m", "main notes")
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "remove-skipped", linked)
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	successful(t, linked, "init", "--from", "main")
	// The linked workspace saves its own copy before the policy exists.
	successful(t, linked, "commit", "-m", "linked notes")
	// A policy from an earlier release spelled the rule differently.
	common := filepath.Join(repo.Root, ".git")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, repo.Root, common, []secureblob.PathRule{{Path: "notes/secret.txt", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := storage.OpenDir(common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(dir, policy, repo.Root, common); err != nil {
		t.Fatal(err)
	}
	dir.Close()
	return repo, linked, write
}

// A snapshot refuses a file that a policy from an earlier release covers only
// by spelling, so removal, which saves the worktree first, is refused with
// the correction command and the worktree is kept, even with --force.
func TestWorkspaceRemoveRefusesWhenSnapshotRefusesAliasedFile(t *testing.T) {
	t.Parallel()
	repo, linked, write := skippedRemoveFixture(t)
	write(linked, "notes/Secret.txt", "TOKEN=unsaved\n")
	for _, args := range [][]string{{"workspace", "remove", linked}, {"workspace", "remove", linked, "--force"}} {
		code, out, diagnostic := callCLI(t, repo.Root, args...)
		if code == 0 || !strings.Contains(diagnostic, `"notes/Secret.txt" matches sensitive path "notes/secret.txt"`) || !strings.Contains(diagnostic, "git local encrypt migrate") {
			t.Fatalf("%v (exit %d):\n%s\n%s", args, code, out, diagnostic)
		}
		if data, err := os.ReadFile(filepath.Join(linked, "notes/Secret.txt")); err != nil || string(data) != "TOKEN=unsaved\n" {
			t.Fatalf("%v removed the worktree although its snapshot was refused: %q %v", args, data, err)
		}
	}
}

// unavailableWorkspaceFixture gives a repository with a tracked file and an
// initialized linked worktree that was then moved away without Git, so every
// snapshot in the main worktree warns that its rules can't be verified.
func unavailableWorkspaceFixture(t *testing.T) (*testutil.Repo, func(root, path, content string)) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	write := func(root, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(repo.Root, "notes/plain.txt", "plain v1\n")
	successful(t, repo.Root, "init")
	successful(t, repo.Root, "track", "notes")
	successful(t, repo.Root, "commit", "-m", "main notes")
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "moved-away", linked)
	successful(t, linked, "init", "--from", "main")
	if err := os.Rename(linked, linked+"-moved"); err != nil {
		t.Fatal(err)
	}
	return repo, write
}
