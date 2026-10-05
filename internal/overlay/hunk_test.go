package overlay

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

func TestContainsPatchCatchesEachHunkOnItsOwn(t *testing.T) {
	git := gitPath(t)
	ctx := context.Background()
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = none\ntail")
	local := []byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = s3cret\ntail")
	v := Version{Base: base, Local: local}
	for name, tc := range map[string]struct {
		candidate []byte
		leak      bool
	}{
		"clean":         {base, false},
		"unrelated":     {[]byte("port = 80\na\nb\nCHANGED\nd\ne\nf\npassword = none\ntail"), false},
		"whole":         {local, true},
		"port only":     {[]byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = none\ntail"), true},
		"password only": {[]byte("port = 80\na\nb\nc\nd\ne\nf\npassword = s3cret\ntail"), true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ContainsPatch(ctx, git, v, tc.candidate)
			if err != nil || got != tc.leak {
				t.Fatalf("ContainsPatch = %v, %v; want %v", got, err, tc.leak)
			}
		})
	}
	versions, err := hunkVersions(ctx, git, base, local)
	if err != nil || len(versions) != 2 {
		t.Fatalf("hunks: %d %v", len(versions), err)
	}
	if !bytes.Equal(versions[0], []byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = none\ntail")) || !bytes.Equal(versions[1], []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = s3cret\ntail")) {
		t.Fatalf("hunk versions:\n%q\n%q", versions[0], versions[1])
	}
	inserted, err := hunkVersions(ctx, git, []byte("a\nb\n"), []byte("top\na\nb\nend"))
	if err != nil || len(inserted) != 2 || string(inserted[0]) != "top\na\nb\n" || string(inserted[1]) != "a\nb\nend" {
		t.Fatalf("insertions: %q %v", inserted, err)
	}
}

func TestOverlayGuardRejectsOneDirectlyStagedHunk(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = none\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	local := func(args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		return cmd.CombinedOutput()
	}
	for _, args := range [][]string{{"init"}} {
		if out, err := local(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	patch := []byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = s3cret\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"overlay", "add", "dev", "--", "app.conf"}, {"hook", "install"}} {
		if out, err := local(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	// Stage only the password hunk, bypassing the clean filter.
	partial := []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = s3cret\n")
	hash := exec.Command(repo.GitPath, "hash-object", "-w", "--stdin")
	hash.Dir, hash.Env, hash.Stdin = repo.Root, repo.Env, bytes.NewReader(partial)
	object, err := hash.CombinedOutput()
	if err != nil {
		t.Fatalf("hash: %v\n%s", err, object)
	}
	repo.Git("update-index", "--cacheinfo", "100644,"+string(bytes.TrimSpace(object))+",app.conf")
	if out, err := local("guard", "staged"); err == nil || !bytes.Contains(out, []byte("overlay \"dev\"")) {
		t.Fatalf("guard staged missed a partial overlay: %v\n%s", err, out)
	}
	commit := exec.Command(repo.GitPath, "commit", "-qm", "leak")
	commit.Dir, commit.Env = repo.Root, repo.Env
	if out, err := commit.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("overlay \"dev\"")) {
		t.Fatalf("pre-commit missed a partial overlay: %v\n%s", err, out)
	}
	repo.Git("commit", "--no-verify", "-qm", "leak")
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	push := exec.Command(repo.GitPath, "push", "origin", "main")
	push.Dir, push.Env = repo.Root, repo.Env
	if out, err := push.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("overlay \"dev\"")) {
		t.Fatalf("pre-push missed a partial overlay: %v\n%s", err, out)
	}
	// A clean commit of an ordinary edit still passes.
	repo.Git("reset", "-q", "--hard", "HEAD~1")
	edited := []byte("port = 8080\na\nb\nc2\nd\ne\nf\npassword = s3cret\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), edited, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	clean := exec.Command(repo.GitPath, "commit", "-qm", "ordinary edit")
	clean.Dir, clean.Env = repo.Root, repo.Env
	if out, err := clean.CombinedOutput(); err != nil {
		t.Fatalf("ordinary edit refused: %v\n%s", err, out)
	}
	if got := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(got, []byte("port = 80\na\nb\nc2\nd\ne\nf\npassword = none\n")) {
		t.Fatalf("committed content: %q", got)
	}
}
