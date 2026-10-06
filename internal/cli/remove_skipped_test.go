package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
)

func TestWorkspaceRemoveRefusesSkippedChangesUnlessForced(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
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
	write(linked, "notes/Secret.txt", "TOKEN=unsaved\n")
	code, out, diagnostic := callCLI(t, repo.Root, "workspace", "remove", linked)
	if code == 0 || !strings.Contains(diagnostic, `skipped "notes/Secret.txt"`) || !strings.Contains(diagnostic, "Warning:") || !strings.Contains(diagnostic, "--force") {
		t.Fatalf("removal with a skipped change (exit %d):\n%s\n%s", code, out, diagnostic)
	}
	if _, err := os.Stat(filepath.Join(linked, "notes/Secret.txt")); err != nil {
		t.Fatalf("worktree removed although a skipped file was unsaved: %v", err)
	}
	code, out, _ = callCLI(t, repo.Root, "workspace", "remove", linked, "--json")
	var envelope struct {
		OK       bool     `json:"ok"`
		Warnings []string `json:"warnings"`
		Data     struct {
			Removed bool `json:"removed"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || code == 0 || envelope.OK || envelope.Data.Removed || len(envelope.Warnings) != 1 || !strings.Contains(envelope.Warnings[0], `skipped "notes/Secret.txt"`) {
		t.Fatalf("JSON removal with a skipped change (exit %d): %s %v", code, out, err)
	}
	code, out, diagnostic = callCLI(t, repo.Root, "workspace", "remove", linked, "--force")
	if code != 0 || !strings.Contains(diagnostic, `skipped "notes/Secret.txt"`) || !strings.Contains(out, "Removed and retained workspace") {
		t.Fatalf("forced removal (exit %d):\n%s\n%s", code, out, diagnostic)
	}
	if _, err := os.Stat(linked); !os.IsNotExist(err) {
		t.Fatalf("forced removal left the worktree: %v", err)
	}
}
