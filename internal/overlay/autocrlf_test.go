package overlay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

// TestOverlayRefusesLineEndingConversion: Git for Windows defaults to
// core.autocrlf=true, which makes the saved patch the whole file. overlay add
// must refuse it, whether set locally or globally, and doctor must flag an
// existing overlay once it is set.
func TestOverlayRefusesLineEndingConversion(t *testing.T) {
	repo := testutil.New(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte("port = 80\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	env := repo.RunnerEnv()
	home := filepath.Join(repo.Root, "fixture-home")
	local := func(args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, env
		return cmd.CombinedOutput()
	}
	if out, err := local("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte("port = 8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"true", "input"} {
		repo.Git("config", "core.autocrlf", value)
		if out, err := local("overlay", "add", "dev", "--", "app.conf"); err == nil || !bytes.Contains(out, []byte("core.autocrlf="+value)) || !bytes.Contains(out, []byte("git config core.autocrlf false")) {
			t.Fatalf("overlay add with core.autocrlf=%s: %v\n%s", value, err, out)
		}
	}
	repo.Git("config", "--unset", "core.autocrlf")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\tautocrlf = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := local("overlay", "add", "dev", "--", "app.conf"); err == nil || !bytes.Contains(out, []byte("core.autocrlf=true")) {
		t.Fatalf("overlay add with a global core.autocrlf=true: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, ".git", "info", "attributes")); !os.IsNotExist(err) {
		t.Fatalf("refused overlay add wrote attributes: %v", err)
	}
	// An overlay saved without conversion, then the setting changes.
	repo.Git("config", "core.autocrlf", "false")
	if out, err := local("overlay", "add", "dev", "--", "app.conf"); err != nil {
		t.Fatalf("overlay add with core.autocrlf=false: %v\n%s", err, out)
	}
	repo.Git("config", "--unset", "core.autocrlf")
	out, err := local("doctor")
	if err == nil || !bytes.Contains(out, []byte("core.autocrlf=true")) || !bytes.Contains(out, []byte("app.conf")) {
		t.Fatalf("doctor with an overlay and core.autocrlf=true: %v\n%s", err, out)
	}
}
