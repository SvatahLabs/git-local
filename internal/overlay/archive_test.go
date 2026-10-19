package overlay

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// archiveOverride is the documented git -c override that keeps git archive
// from smudging active overlays into the archive.
var archiveOverride = []string{"-c", "filter.gitlocal-overlay.process=", "-c", "filter.gitlocal-overlay.required=false"}

func archived(t *testing.T, repo *testutil.Repo, prefix []string, name string) []byte {
	t.Helper()
	args := append(append([]string{}, prefix...), "archive", "--format=tar", "HEAD")
	cmd := exec.Command(repo.GitPath, args...)
	cmd.Dir, cmd.Env = repo.Root, repo.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git archive: %v\n%s", err, stderr.Bytes())
	}
	if stderr.Len() != 0 {
		t.Fatalf("git archive warned: %s", stderr.Bytes())
	}
	reader := tar.NewReader(bytes.NewReader(out))
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("%s not in archive: %v", name, err)
		}
		if header.Name == name {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
}

func TestDocumentedArchiveOverrideExcludesOverlay(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	local := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git-local %v: %v\n%s", args, err, out)
		}
	}
	local("init")
	patch := []byte("port = 8080\na\nb\nc\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	local("overlay", "add", "port", "--", "app.conf")
	// The risk the documentation warns about: plain git archive smudges.
	if got := archived(t, repo, nil, "app.conf"); !bytes.Equal(got, patch) {
		t.Fatalf("plain git archive no longer applies the overlay (update the docs): %q", got)
	}
	if got := archived(t, repo, archiveOverride, "app.conf"); !bytes.Equal(got, base) {
		t.Fatalf("documented archive override kept the overlay: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(got, patch) {
		t.Fatalf("working file changed: %q %v", got, err)
	}
}
