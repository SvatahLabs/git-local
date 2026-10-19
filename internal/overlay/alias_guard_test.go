//go:build !windows

package overlay

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// TestOverlayGuardChecksBackslashAliasSpelling stages content under conf\app.conf,
// a name Windows resolves to the overlay's conf/app.conf. The guard must
// check that content like any other staged copy, not fail on the spelling.
func TestOverlayGuardChecksBackslashAliasSpelling(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = none\n")
	if err := os.Mkdir(filepath.Join(repo.Root, "conf"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "conf", "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "conf/app.conf")
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
	patch := []byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = s3cret\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "conf", "app.conf"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := local("overlay", "add", "dev", "--", "conf/app.conf"); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	}
	stage := func(content []byte) {
		t.Helper()
		hash := exec.Command(repo.GitPath, "hash-object", "-w", "--stdin")
		hash.Dir, hash.Env, hash.Stdin = repo.Root, repo.Env, bytes.NewReader(content)
		object, err := hash.CombinedOutput()
		if err != nil {
			t.Fatalf("hash: %v\n%s", err, object)
		}
		repo.Git("update-index", "--add", "--cacheinfo", "100644,"+string(bytes.TrimSpace(object))+`,conf\app.conf`)
	}
	stage(patch)
	out, err := local("guard", "staged")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !bytes.Contains(out, []byte(`overlay "dev"`)) || !bytes.Contains(out, []byte(`conf\\app.conf`)) {
		t.Fatalf("patched content under an alias spelling: %v\n%s", err, out)
	}
	stage(base)
	if out, err := local("guard", "staged"); err != nil {
		t.Fatalf("clean content under an alias spelling: %v\n%s", err, out)
	}
}
