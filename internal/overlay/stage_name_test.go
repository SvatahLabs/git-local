//go:build !windows

package overlay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// TestOverlayOnFileNamedLikeAStage: ":0:foo" names a stage, so reading the
// index blob of a file named "0:foo" as ":"+path gets "foo" instead. An
// overlay on 0:foo must use 0:foo's own content as its base.
func TestOverlayOnFileNamedLikeAStage(t *testing.T) {
	repo := testutil.New(t)
	for name, content := range map[string]string{"foo": "the other file\n", "0:foo": "port = 80\na\nb\nc\nname = app\n"} {
		if err := os.WriteFile(filepath.Join(repo.Root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	repo.Git("add", "--", "foo", "0:foo")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	local := func(args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		return cmd.CombinedOutput()
	}
	if out, err := local("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "0:foo"), []byte("port = 8080\na\nb\nc\nname = app\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := local("overlay", "add", "dev", "--", "0:foo"); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	}
	// An ordinary edit beside the patch is staged on 0:foo's own content.
	if err := os.WriteFile(filepath.Join(repo.Root, "0:foo"), []byte("port = 8080\na\nb\nc\nname = renamed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "--", "0:foo")
	if staged := repo.Git("cat-file", "blob", ":0:0:foo"); !bytes.Equal(staged, []byte("port = 80\na\nb\nc\nname = renamed\n")) {
		t.Fatalf("staged 0:foo: %q", staged)
	}
	if out, err := local("guard", "staged"); err != nil {
		t.Fatalf("guard: %v\n%s", err, out)
	}
}
