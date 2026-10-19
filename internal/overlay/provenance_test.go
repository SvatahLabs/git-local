package overlay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// Leftovers of this extension's own earlier installs are recorded as ours, so
// the last removal cleans them up instead of keeping them forever.
func TestReinstallDoesNotAdoptOwnLeftoversAsUserConfig(t *testing.T) {
	ctx := context.Background()
	repo := testutil.New(t)
	path := filepath.Join(repo.Root, "app.conf")
	if err := os.WriteFile(path, []byte("port = 80\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), filepath.Base(source))
	program, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, program, 0700); err != nil {
		t.Fatal(err)
	}
	local := func(args ...string) string {
		out, _ := localConfig(ctx, r, repo.Root, args...)
		return strings.TrimSpace(string(out))
	}
	cycle := func(leftover func()) {
		t.Helper()
		if err := os.WriteFile(path, []byte("port = 8080\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Add(ctx, r, repo.Root, "port", "app.conf", executable); err != nil {
			t.Fatal(err)
		}
		if _, err := Remove(ctx, r, repo.Root, "port"); err != nil {
			t.Fatal(err)
		}
		if got := local("--get-all", "include.path"); got != "" {
			t.Fatalf("include left after removal: %q", got)
		}
		if got := local("--get-all", requiredKey); got != "" {
			t.Fatalf("%s left after removal: %q", requiredKey, got)
		}
		leftover()
	}
	cycle(func() {
		// An interrupted removal unset the include but not the required key.
		repo.Git("config", "--local", requiredKey, "true")
	})
	cycle(func() {
		// An earlier release's absolute include is still present.
		repo.Git("config", "--local", "--add", "include.path", filepath.Join(w.Common, "gitlocal", filterConfigName))
	})
	cycle(func() {})
}
