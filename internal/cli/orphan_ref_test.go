package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorAndAbandonResolveOrphanRecoveryRef plants the recovery ref a
// restore killed before its journal leaves behind. Doctor names it with the
// command that removes it, recover --abandon removes it, and encryption
// migration, which refuses unknown private refs, then succeeds.
func TestDoctorAndAbandonResolveOrphanRecoveryRef(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	root := repo.Root
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOKEN=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, root, "track", "secret.txt")
	successful(t, root, "commit", "-m", "plaintext")
	workspaceID := listWorkspaces(t, root)[0].ID
	store := "--git-dir=" + filepath.Join(root, ".git", "gitlocal", "history.git")
	id := "0b8f6c2e-1d4a-4e5f-9a7b-3c2d1e0f4a5b"
	tree := strings.TrimSpace(string(repo.Git(store, "mktree")))
	commit := strings.TrimSpace(string(repo.Git(store, "commit-tree", tree, "-m", "Git Local restore recovery "+id)))
	ref := "refs/gitlocal/recovery/" + workspaceID + "/" + id
	repo.Git(store, "update-ref", ref, commit)

	code, out, _ := callCLI(t, root, "doctor")
	if !strings.Contains(out, ref) || !strings.Contains(out, "git local recover "+id+" --abandon") {
		t.Fatalf("doctor (exit %d):\n%s", code, out)
	}
	if out := successful(t, root, "recover", id, "--abandon"); !strings.Contains(out, "Removed recovery ref "+ref) {
		t.Fatalf("abandon:\n%s", out)
	}
	if refs := string(repo.Git(store, "for-each-ref", "refs/gitlocal/recovery/")); refs != "" {
		t.Fatalf("recovery refs remain: %s", refs)
	}
	if code, _, diagnostic := callCLI(t, root, "recover", id, "--abandon"); code == 0 || !strings.Contains(diagnostic, "no transaction "+id) {
		t.Fatalf("second abandon: exit %d %s", code, diagnostic)
	}
	key := filepath.Join(t.TempDir(), "sensitive.key")
	successful(t, root, "encrypt", "keygen", key)
	successful(t, root, "encrypt", "migrate", "--key-file", key, "--", "secret.txt")
}
