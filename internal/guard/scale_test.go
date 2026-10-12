package guard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
)

// importChain adds a linear history of n commits on branch from main with
// git fast-import; commit leak (1-based) also adds the selected notes.local.
func importChain(t *testing.T, repo *testutil.Repo, branch string, n, leak int) []string {
	t.Helper()
	main := strings.TrimSpace(string(repo.Git("rev-parse", "main")))
	var script strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&script, "commit refs/heads/%s\nmark :%d\ncommitter Fixture <fixture@localhost> %d +0000\ndata 7\nchange\n", branch, i, 1700000000+i)
		if i == 1 {
			fmt.Fprintf(&script, "from %s\n", main)
		} else {
			fmt.Fprintf(&script, "from :%d\n", i-1)
		}
		content := fmt.Sprintf("bulk %d\n", i)
		fmt.Fprintf(&script, "M 100644 inline bulk.txt\ndata %d\n%s\n", len(content), content)
		if i == leak {
			fmt.Fprintf(&script, "M 100644 inline notes.local\ndata 6\nleak!\n\n")
		}
	}
	cmd := exec.Command(repo.GitPath, "fast-import", "--quiet")
	cmd.Dir, cmd.Env, cmd.Stdin = repo.Root, repo.Env, strings.NewReader(script.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v %s", err, out)
	}
	return strings.Fields(string(repo.Git("rev-list", "--reverse", "main.."+branch)))
}

// Doctor's audit of 10k commits shared by ten tracking branches, and a
// pre-push of 10k refs (over the old 1 MiB input limit), each run a fixed
// number of Git processes instead of one per commit or per ref.
func TestScansUseFixedGitProcessesForLargeHistories(t *testing.T) {
	repo, r, ctx := guardRepo(t, "origin")
	const commits = 10000
	chain := importChain(t, repo, "bulk", commits, commits/2)
	if len(chain) != commits {
		t.Fatalf("imported %d commits", len(chain))
	}
	for i := 0; i < 10; i++ {
		branch := fmt.Sprintf("track-%d", i)
		repo.Git("branch", branch, chain[commits-1-i*100])
		repo.Git("config", "branch."+branch+".remote", "origin")
		repo.Git("config", "branch."+branch+".merge", "refs/heads/"+branch)
	}
	before := r.SubprocessCount()
	findings, err := AuditUpstream(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	spent := r.SubprocessCount() - before
	if len(findings) != 1 || findings[0].Commit != chain[commits/2-1] || findings[0].Path != "notes.local" {
		t.Fatalf("audit findings: %+v", findings)
	}
	if spent > 40 {
		t.Fatalf("audit of %d commits ran %d Git processes", commits, spent)
	}
	t.Logf("audit: %d Git processes", spent)
	var input strings.Builder
	zero := strings.Repeat("0", len(chain[0]))
	for i := 0; i < commits; i++ {
		fmt.Fprintf(&input, "refs/heads/ref-%05d %s refs/heads/ref-%05d %s\n", i, chain[i], i, zero)
	}
	if input.Len() <= 1<<20 {
		t.Fatalf("pre-push input is only %d bytes", input.Len())
	}
	before = r.SubprocessCount()
	findings, err = Push(ctx, r, repo.Root, "origin", strings.NewReader(input.String()))
	if err != nil {
		t.Fatalf("pre-push of %d refs: %v", commits, err)
	}
	spent = r.SubprocessCount() - before
	if len(findings) != 1 || findings[0].Commit != chain[commits/2-1] {
		t.Fatalf("pre-push findings: %+v", findings)
	}
	if spent > 40 {
		t.Fatalf("pre-push of %d refs ran %d Git processes", commits, spent)
	}
	t.Logf("pre-push: %d Git processes", spent)
}

// A force-updated annotated tag's old remote value is the tag object, which
// peels to a commit with another ID; it must still count as known, so its
// history isn't scanned again.
func TestKnownCommitsRecognizesAnnotatedTags(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	repo.Git("tag", "-a", "-m", "release", "v1")
	tag := strings.TrimSpace(string(repo.Git("rev-parse", "v1")))
	commit := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	missing := strings.Repeat("1", len(commit))
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	known, err := knownCommits(context.Background(), r, repo.Root, []string{tag, missing, commit})
	if err != nil {
		t.Fatal(err)
	}
	if !known[tag] || !known[commit] || known[missing] || len(known) != 2 {
		t.Fatalf("known commits: %v (tag %s, commit %s)", known, tag, commit)
	}
}

// An earlier committed version too large to be overlay text is left out of
// the adopted versions rather than read, so a version over Git's output cap
// doesn't make the commit and push guards fail.
func TestCommittedBlobsSkipsVersionsLargerThanOverlayText(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	big := bytes.Repeat([]byte("x"), gitexec.MaxOutput+1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.json"), big, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.json")
	repo.Git("commit", "-qm", "large version")
	if err := os.WriteFile(filepath.Join(repo.Root, "small.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "small.json")
	repo.Git("commit", "-qm", "small file")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	blobs, err := committedBlobs(context.Background(), r, repo.Root, "app.json", "HEAD", "HEAD~1")
	if err != nil || len(blobs) != 0 {
		t.Fatalf("large versions: %d blobs, %v", len(blobs), err)
	}
	blobs, err = committedBlobs(context.Background(), r, repo.Root, "small.json", "HEAD")
	if err != nil || len(blobs) != 1 || string(blobs[0]) != "{}\n" {
		t.Fatalf("small version: %q %v", blobs, err)
	}
}
