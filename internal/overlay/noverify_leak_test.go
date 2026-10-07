package overlay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

// TestPreCommitRefusesRestagingAfterNoVerifyLeak: once git commit
// --no-verify has leaked an overlay hunk, HEAD carries it. The next commit
// that stages the file again must still be refused, not treated as if the
// hunk had been adopted upstream.
func TestPreCommitRefusesRestagingAfterNoVerifyLeak(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = none\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	run := func(name string, args ...string) ([]byte, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.Env
		if name == bin {
			cmd.Env = repo.RunnerEnv()
		}
		return cmd.CombinedOutput()
	}
	if out, err := run(bin, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	patch := []byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = s3cret\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"overlay", "add", "dev", "--", "app.conf"}, {"hook", "install"}} {
		if out, err := run(bin, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	// Leak the password hunk with --no-verify, bypassing the clean filter.
	leaked := []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = s3cret\n")
	hash := exec.Command(repo.GitPath, "hash-object", "-w", "--stdin")
	hash.Dir, hash.Env, hash.Stdin = repo.Root, repo.Env, bytes.NewReader(leaked)
	object, err := hash.CombinedOutput()
	if err != nil {
		t.Fatalf("hash: %v\n%s", err, object)
	}
	repo.Git("update-index", "--cacheinfo", "100644,"+string(bytes.TrimSpace(object))+",app.conf")
	repo.Git("commit", "--no-verify", "-qm", "leak")
	// The next commit stages the file again, with an unrelated edit.
	edited := bytes.Replace(leaked, []byte("\nc\n"), []byte("\nc2\n"), 1)
	hash = exec.Command(repo.GitPath, "hash-object", "-w", "--stdin")
	hash.Dir, hash.Env, hash.Stdin = repo.Root, repo.Env, bytes.NewReader(edited)
	if object, err = hash.CombinedOutput(); err != nil {
		t.Fatalf("hash: %v\n%s", err, object)
	}
	repo.Git("update-index", "--cacheinfo", "100644,"+string(bytes.TrimSpace(object))+",app.conf")
	if out, err := run(repo.GitPath, "commit", "-qm", "edit after the leak"); err == nil || !bytes.Contains(out, []byte(`overlay "dev"`)) {
		t.Fatalf("pre-commit passed a commit that still carries the leaked hunk: %v\n%s", err, out)
	}
	// Once the leak is pushed, the hunk is upstream: a commit on top of it is
	// no longer refused for it.
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	repo.Git("push", "-q", "--no-verify", "origin", "main")
	repo.Git("fetch", "-q", "origin")
	if out, err := run(repo.GitPath, "commit", "-qm", "edit after the leak was published"); err != nil {
		t.Fatalf("commit on top of a published hunk: %v\n%s", err, out)
	}
}
