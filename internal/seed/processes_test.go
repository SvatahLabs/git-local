package seed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/keymaterial"
	"github.com/SvatahLabs/git-local/internal/secureblob"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// seedProcesses seeds n sensitive files, n sensitive symlinks and n plain
// symlinks into a new linked worktree, checks every one, resumes the seed's
// journal once more (recovery verifies each encrypted file again), and
// returns the Git processes the seed and the resume ran.
func seedProcesses(t *testing.T, n int) (uint64, uint64) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	main, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"secret", "links"} {
		if err := os.MkdirAll(filepath.Join(repo.Root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(repo.Root, "secret", fmt.Sprintf("file-%03d", i)), []byte(fmt.Sprintf("TOKEN=%d\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fmt.Sprintf("file-%03d", i), filepath.Join(repo.Root, "secret", fmt.Sprintf("link-%03d", i))); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fmt.Sprintf("../secret/file-%03d", i), filepath.Join(repo.Root, "links", fmt.Sprintf("link-%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"secret", "links"} {
		if _, err := configtx.Change(ctx, r, repo.Root, true, dir); err != nil {
			t.Fatal(err)
		}
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, main.Root, main.Common, []secureblob.PathRule{{Path: "secret", Kind: "directory"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(main.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, main.Root, main.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	if _, err := history.Commit(ctx, r, repo.Root, "encrypted source"); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed-processes", linked)
	before := r.SubprocessCount()
	seeded, err := Run(ctx, r, linked, main.WorkspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	seeding := r.SubprocessCount() - before
	for i := 0; i < n; i++ {
		name := filepath.Join(linked, "secret", fmt.Sprintf("file-%03d", i))
		if b, err := os.ReadFile(name); err != nil || string(b) != fmt.Sprintf("TOKEN=%d\n", i) {
			t.Fatalf("%s: %q %v", name, b, err)
		}
		for _, link := range []struct{ name, target string }{
			{filepath.Join(linked, "secret", fmt.Sprintf("link-%03d", i)), fmt.Sprintf("file-%03d", i)},
			{filepath.Join(linked, "links", fmt.Sprintf("link-%03d", i)), fmt.Sprintf("../secret/file-%03d", i)},
		} {
			if target, err := os.Readlink(link.name); err != nil || target != link.target {
				t.Fatalf("%s: %q %v", link.name, target, err)
			}
		}
	}
	reopenCompletedSeedJournal(t, main.Common, seeded.TransactionID)
	before = r.SubprocessCount()
	if _, err := Recover(ctx, r, linked, seeded.TransactionID, false); err != nil {
		t.Fatal(err)
	}
	return seeding, r.SubprocessCount() - before
}

// Sensitive files and symlinks are read through the seed's one cat-file
// process, and the planner and recovery verify every encrypted file through
// one more, so the Git processes a seed runs don't grow with them. Before,
// each sensitive file ran two Git processes (verification and decryption) and
// each symlink one (GL-120).
func TestSeedingSensitiveFilesAndSymlinksRunsConstantGitProcesses(t *testing.T) {
	t.Parallel()
	smallSeed, smallResume := seedProcesses(t, 4)
	largeSeed, largeResume := seedProcesses(t, 24)
	t.Logf("Git processes: seed 4x3 files %d, 24x3 files %d; resume %d, %d", smallSeed, largeSeed, smallResume, largeResume)
	if largeSeed > smallSeed+4 || largeResume > smallResume+4 {
		t.Fatalf("60 more seeded files ran %d more Git processes seeding and %d more resuming", largeSeed-smallSeed, largeResume-smallResume)
	}
}
