//go:build !windows

package overlay

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

// TestOverlayAddFromSymlinkedDirectory runs overlay add from a working
// directory reached through a symlink, as for a repository under /tmp on
// macOS: the logical $PWD differs from Git's physical root.
func TestOverlayAddFromSymlinkedDirectory(t *testing.T) {
	repo := testutil.New(t)
	if err := os.MkdirAll(filepath.Join(repo.Root, "conf"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.conf", "conf/db.conf"} {
		if err := os.WriteFile(filepath.Join(repo.Root, name), []byte("port = 80\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	repo.Git("add", "app.conf", "conf/db.conf")
	repo.Git("commit", "-qm", "initial")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo.Root, link); err != nil {
		t.Fatal(err)
	}
	bin := integrationBinary(t)
	local := func(dir string, args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = dir, append(repo.RunnerEnv(), "PWD="+dir)
		return cmd.CombinedOutput()
	}
	if out, err := local(link, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, name := range []string{"app.conf", "conf/db.conf"} {
		if err := os.WriteFile(filepath.Join(repo.Root, name), []byte("port = 8080\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := local(link, "overlay", "add", "app", "--", "app.conf"); err != nil {
		t.Fatalf("overlay add from a symlinked root: %v\n%s", err, out)
	}
	if out, err := local(filepath.Join(link, "conf"), "overlay", "add", "db", "--", "db.conf"); err != nil {
		t.Fatalf("overlay add from a symlinked subdirectory: %v\n%s", err, out)
	}
	if out, err := local(link, "overlay", "status"); err != nil {
		t.Fatalf("overlay status: %v\n%s", err, out)
	}
}
