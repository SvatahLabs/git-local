package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// A checkout in a repository git-local was never initialized in is silent:
// with `auto install` into a shared hooks directory, the post-checkout event
// runs in unrelated repositories, in their main checkout and in linked
// worktrees alike. Before, it failed there with "main checkout identity is
// missing" or "initialize the main checkout", which the hook printed as a
// warning on every checkout.
func TestAutomaticPostCheckoutIsSilentWithoutState(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "--quiet", "-b", "linked", linked)
	for _, cwd := range []string{repo.Root, linked} {
		code, out, diagnostic := callCLI(t, cwd, "auto", "event", "post-checkout")
		if code != 0 || out != "" || diagnostic != "" {
			t.Fatalf("auto event post-checkout in %s: exit %d\n%s%s", cwd, code, out, diagnostic)
		}
	}
	if _, err := os.Lstat(filepath.Join(repo.Root, ".git", "gitlocal")); !os.IsNotExist(err) {
		t.Fatalf("auto event created git-local state: %v", err)
	}
}
