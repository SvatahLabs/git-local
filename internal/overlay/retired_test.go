package overlay

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

// TestOverlayOfRemovedWorktreeCanBeRemoved: a linked worktree adds an overlay
// and is then removed. Its attribute line, filter include and required key
// must still be removable, from another worktree, once it is retained.
func TestOverlayOfRemovedWorktreeCanBeRemoved(t *testing.T) {
	repo := testutil.New(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte("port = 80\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	at := func(dir string, args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = dir, repo.RunnerEnv()
		return cmd.CombinedOutput()
	}
	must := func(dir string, args ...string) []byte {
		t.Helper()
		out, err := at(dir, args...)
		if err != nil {
			t.Fatalf("%v in %s: %v\n%s", args, dir, err, out)
		}
		return out
	}
	must(repo.Root, "init")
	addLinked := func(name string) (string, string) {
		t.Helper()
		linked := filepath.Join(t.TempDir(), name)
		repo.Git("worktree", "add", "-q", "-b", name, linked)
		var envelope struct {
			Data struct {
				WorkspaceID string `json:"workspaceId"`
			} `json:"data"`
		}
		if err := json.Unmarshal(must(linked, "init", "--json"), &envelope); err != nil || envelope.Data.WorkspaceID == "" {
			t.Fatalf("linked init: %v", err)
		}
		if err := os.WriteFile(filepath.Join(linked, "app.conf"), []byte("port = 8080\n"), 0600); err != nil {
			t.Fatal(err)
		}
		must(linked, "overlay", "add", name, "--", "app.conf")
		return linked, envelope.Data.WorkspaceID
	}
	linked, id := addLinked("gone")
	if attributes, err := os.ReadFile(filepath.Join(repo.Root, ".git", "info", "attributes")); err != nil || !bytes.Contains(attributes, []byte("app.conf")) {
		t.Fatalf("overlay attributes: %q %v", attributes, err)
	}
	must(repo.Root, "workspace", "remove", "--force", linked)
	if out, err := at(repo.Root, "overlay", "remove", "gone"); err == nil {
		t.Fatalf("removed another workspace's overlay without --workspace:\n%s", out)
	}
	must(repo.Root, "overlay", "remove", "gone", "--workspace", id)
	if _, err := os.Stat(filepath.Join(repo.Root, ".git", "info", "attributes")); !os.IsNotExist(err) {
		t.Fatalf("attributes remain after the last overlay: %v", err)
	}
	if config := repo.Git("config", "--local", "--list"); bytes.Contains(config, []byte("include.path")) || bytes.Contains(config, []byte("gitlocal")) {
		t.Fatalf("overlay configuration remains after the last overlay:\n%s", config)
	}
	must(repo.Root, "overlay", "remove", "gone", "--workspace", id)
	// An active workspace's overlay is removed only from its own worktree.
	active, activeID := addLinked("active")
	if out, err := at(repo.Root, "overlay", "remove", "active", "--workspace", activeID); err == nil || !bytes.Contains(out, []byte("active")) {
		t.Fatalf("removed an active workspace's overlay from elsewhere: %v\n%s", err, out)
	}
	must(active, "overlay", "remove", "active")
}
