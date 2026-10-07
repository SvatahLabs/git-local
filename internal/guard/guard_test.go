package guard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// guardRepo is an initialized repository that selects notes.local, with a
// bare remote that holds its first commit.
func guardRepo(t *testing.T, remote string) (*testutil.Repo, *gitexec.Runner, context.Context) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("local only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "notes.local"); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", bare)
	repo.Git("remote", "add", remote, bare)
	repo.Git("push", "-q", "-u", remote, "main")
	return repo, r, ctx
}

func commitFile(t *testing.T, repo *testutil.Repo, path, content string, force bool) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.Root, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if force {
		repo.Git("add", "-f", path)
	} else {
		repo.Git("add", path)
	}
	repo.Git("commit", "-qm", "change "+path)
	return strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
}

// TestPushWithUnknownRemoteOldCommit: after someone else pushed, a force push
// without fetching names a remote old commit this repository doesn't have.
// The guard must still check the pushed commits instead of failing.
func TestPushWithUnknownRemoteOldCommit(t *testing.T) {
	repo, r, ctx := guardRepo(t, "origin")
	unknown := strings.Repeat("ab", 20)
	clean := commitFile(t, repo, "app.txt", "clean change\n", false)
	findings, err := Push(ctx, r, repo.Root, "origin", strings.NewReader("refs/heads/main "+clean+" refs/heads/main "+unknown+"\n"))
	if err != nil || len(findings) != 0 {
		t.Fatalf("clean force push: %+v %v", findings, err)
	}
	leak := commitFile(t, repo, "notes.local", "leaked\n", true)
	findings, err = Push(ctx, r, repo.Root, "origin", strings.NewReader("refs/heads/main "+leak+" refs/heads/main "+unknown+"\n"))
	if err != nil || len(findings) != 1 || findings[0].Commit != leak || findings[0].Path != "notes.local" {
		t.Fatalf("leaking force push: %+v %v", findings, err)
	}
	// Pushing to a URL has no remote-tracking refs to exclude: the whole
	// ancestry is checked.
	findings, err = Push(ctx, r, repo.Root, "https://example.invalid/repo.git", strings.NewReader("refs/heads/main "+clean+" refs/heads/main "+unknown+"\n"))
	if err != nil || len(findings) != 0 {
		t.Fatalf("clean force push to a URL: %+v %v", findings, err)
	}
}

// TestAuditUpstreamScansRemoteNamedUpstream: doctor's audit promises a full
// scan, so a leak already pushed to a remote that happens to be named
// "upstream" must still be found.
func TestAuditUpstreamScansRemoteNamedUpstream(t *testing.T) {
	repo, r, ctx := guardRepo(t, "upstream")
	leak := commitFile(t, repo, "notes.local", "leaked\n", true)
	repo.Git("push", "-q", "upstream", "main")
	findings, err := AuditUpstream(ctx, r, repo.Root)
	if err != nil || len(findings) != 1 || findings[0].Commit != leak || findings[0].Path != "notes.local" {
		t.Fatalf("audit: %+v %v", findings, err)
	}
}
