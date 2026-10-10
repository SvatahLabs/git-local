package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// boundSource is a repository with a backup remote whose transports go to a
// shared disposable bare repository.
type boundSource struct {
	repo   *testutil.Repo
	g      *gitexec.Runner
	w      *workspace.Context
	remote string
}

func newBoundSource(t *testing.T, remote, content string) boundSource {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	g, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	ctx := context.Background()
	w, err := workspace.Init(ctx, g, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, g, repo.Root, true, "notes.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, g, repo.Root, "save"); err != nil {
		t.Fatal(err)
	}
	repo.Git("remote", "add", "origin", "git@origin.example:project/main.git")
	repo.Git("remote", "add", "backup", "ssh://git@backup.example/project/archive.git")
	return boundSource{repo: repo, g: g, w: w, remote: remote}
}

func (s boundSource) git(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, s.repo.GitPath, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	cmd.Env = s.repo.RunnerEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s boundSource) push(t *testing.T, keyPath string) string {
	t.Helper()
	result, err := push(context.Background(), s.g, s.repo.Root, "backup", keyPath, func(ctx context.Context, _ *gitexec.Runner, store, _, commit, ref string) error {
		s.git(t, ctx, "--git-dir="+store, "push", "file://"+s.remote, commit+":"+ref)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Ref
}

// fetch requests ref but is served the archive commit that served names, as a
// malicious or compromised backup host could.
func (s boundSource) fetch(t *testing.T, keyPath, ref, served string) (string, error) {
	t.Helper()
	return fetchStaged(context.Background(), s.g, s.repo.Root, "backup", ref, keyPath, func(ctx context.Context, _ *gitexec.Runner, store, _, _ string) error {
		s.git(t, ctx, "--git-dir="+store, "fetch", "--no-tags", "file://"+s.remote, served+":refs/heads/import")
		return nil
	})
}

func TestRestoreRefusesSwappedRefAndForeignArchive(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	source := newBoundSource(t, remote, "older\n")
	source.git(t, context.Background(), "init", "--bare", "-q", remote)
	if err := CreateKeyFile(keyPath, source.repo.Root); err != nil {
		t.Fatal(err)
	}
	older := source.push(t, keyPath)
	if err := os.WriteFile(filepath.Join(source.repo.Root, "notes.local"), []byte("newer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(context.Background(), source.g, source.repo.Root, "newer"); err != nil {
		t.Fatal(err)
	}
	newer := source.push(t, keyPath)
	// Another repository backed up with the same key.
	foreign := newBoundSource(t, remote, "foreign\n").push(t, keyPath)
	if strings.Contains(foreign, source.w.RepositoryID) {
		t.Fatal("fixture repositories share an ID")
	}
	for name, served := range map[string]string{"older archive": older, "foreign archive": foreign} {
		stage, err := source.fetch(t, keyPath, newer, served)
		if err == nil || stage != "" {
			os.RemoveAll(stage)
			t.Fatalf("%s served as %s was accepted: %q %v", name, newer, stage, err)
		}
		if !strings.Contains(err.Error(), "pushed as") {
			t.Fatalf("%s refusal: %v", name, err)
		}
	}
	stage, err := source.fetch(t, keyPath, newer, newer)
	if err != nil {
		t.Fatalf("restore of the requested backup: %v", err)
	}
	defer os.RemoveAll(stage)
	info, err := ValidateStaged(context.Background(), source.g, stage)
	if err != nil || info.RepositoryID != source.w.RepositoryID || info.WorkspaceID != source.w.WorkspaceID {
		t.Fatalf("restored backup: %+v %v", info, err)
	}
	head, err := source.g.History(context.Background(), filepath.Join(stage, "state", "history.git"), nil, "rev-parse", history.Ref(source.w.WorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	if latest, err := history.Resolve(context.Background(), source.g, source.repo.Root, source.w.WorkspaceID, "latest"); err != nil || latest.ID != strings.TrimSpace(string(head)) {
		t.Fatalf("restored backup isn't the newer one: %s %v", head, err)
	}
}
