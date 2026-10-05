package overlay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestNamedVersionsRemainReadableAndBoundToPath(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(context.Background(), r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	base := []byte("port = 80\n")
	first := []byte("port = 8080\n")
	m, err := PublishVersion(context.Background(), w.Common, w.WorkspaceID, "debug-port", "config/app.conf", base, first)
	if err != nil || m.Version != 1 {
		t.Fatalf("first version: %v %+v", err, m)
	}
	second := []byte("port = 9090\n")
	m, err = PublishVersion(context.Background(), w.Common, w.WorkspaceID, "debug-port", "config/app.conf", base, second)
	if err != nil || m.Version != 2 {
		t.Fatalf("second version: %v %+v", err, m)
	}
	v, err := LoadVersion(context.Background(), w.Common, w.WorkspaceID, "debug-port", 1)
	if err != nil || !bytes.Equal(v.Local, first) {
		t.Fatalf("old version changed: %v %q", err, v.Local)
	}
	v, err = LoadVersion(context.Background(), w.Common, w.WorkspaceID, "debug-port", 0)
	if err != nil || !bytes.Equal(v.Local, second) {
		t.Fatalf("active version wrong: %v %q", err, v.Local)
	}
	if _, err = PublishVersion(context.Background(), w.Common, w.WorkspaceID, "debug-port", "other.conf", base, first); err == nil {
		t.Fatal("overlay name moved paths")
	}
	if _, err = PublishVersion(context.Background(), w.Common, w.WorkspaceID, "another", "config/app.conf", base, first); err == nil {
		t.Fatal("second active name claimed path")
	}
	if _, err = workspace.Init(context.Background(), r, repo.Root); err != nil {
		t.Fatalf("overlay metadata broke workspace init: %v", err)
	}
	content := filepath.Join(w.Common, "gitlocal", "overlays", w.WorkspaceID, "debug-port", "v000002", "local")
	if err := os.WriteFile(content, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadVersion(context.Background(), w.Common, w.WorkspaceID, "debug-port", 0); err == nil {
		t.Fatal("corrupted version accepted")
	}
}

func TestProcessorStripsAgainstLiveIndex(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(context.Background(), r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	local := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	if _, err := PublishVersion(context.Background(), w.Common, w.WorkspaceID, "port", "app.conf", base, local); err != nil {
		t.Fatal(err)
	}
	p, err := NewProcessor(context.Background(), r, repo.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	working := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = debug\n")
	clean, err := p.Transform(context.Background(), "clean", "app.conf", working)
	if err != nil || !bytes.Equal(clean, []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = debug\n")) {
		t.Fatalf("live-index clean failed: %v %q", err, clean)
	}
	newBase := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = warn\n")
	smudged, err := p.Transform(context.Background(), "smudge", "app.conf", newBase)
	if err != nil || !bytes.Equal(smudged, []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = warn\n")) {
		t.Fatalf("smudge failed: %v %q", err, smudged)
	}
}

func TestLoadForPathsAcceptsUnselectablePaths(t *testing.T) {
	common := t.TempDir()
	if err := os.MkdirAll(filepath.Join(common, "gitlocal", "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(common, "gitlocal", "locks", "repository.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	versions, err := LoadForPaths(context.Background(), common, "", []string{"aux.c", `a\b.md`, "name.", "\xffbyte.txt"})
	if err != nil || len(versions) != 0 {
		t.Fatalf("LoadForPaths: %v %+v", err, versions)
	}
}
