package configtx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// TestOfflineWorktreeErrorsMakeItUnavailable injects the errors an offline
// network or removable volume reports for a linked worktree's root. The main
// checkout keeps working: discovery, inventory, selection changes and init
// succeed and warn about the unavailable worktree.
func TestOfflineWorktreeErrorsMakeItUnavailable(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ESTALE, syscall.ENOTCONN, syscall.EIO} {
		t.Run(errno.Error(), func(t *testing.T) {
			repo := testutil.New(t)
			repo.Commit()
			r, err := gitexec.New(repo.RunnerEnv())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
				t.Fatal(err)
			}
			linked := filepath.Join(t.TempDir(), "linked")
			repo.Git("worktree", "add", "-q", "-b", "offline", linked)
			if linked, err = filepath.EvalSymlinks(linked); err != nil {
				t.Fatal(err)
			}
			if _, err := workspace.Init(ctx, r, linked); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(linked, "notes.local"), []byte("linked\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Change(ctx, r, linked, true, "notes.local"); err != nil {
				t.Fatal(err)
			}
			stat := workspace.StatRoot
			workspace.StatRoot = func(p string) (os.FileInfo, error) {
				if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved == linked || filepath.Clean(p) == linked {
					return nil, &os.PathError{Op: "stat", Path: p, Err: errno}
				}
				return stat(p)
			}
			defer func() { workspace.StatRoot = stat }()

			if _, err := workspace.Discover(ctx, r, repo.Root); err != nil {
				t.Fatalf("discover: %v", err)
			}
			listed, err := ListWorkspaces(ctx, r, repo.Root)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			unavailable := false
			for _, w := range listed {
				unavailable = unavailable || w.State == "unavailable" && !w.Available
			}
			if !unavailable {
				t.Fatalf("list: %+v", listed)
			}
			if err := os.WriteFile(filepath.Join(repo.Root, "main.local"), []byte("main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := Change(ctx, r, repo.Root, true, "main.local")
			if err != nil || !result.Changed || !strings.Contains(strings.Join(result.Warnings, "\n"), "unavailable") {
				t.Fatalf("track: %+v %v", result, err)
			}
			w, err := workspace.Init(ctx, r, repo.Root)
			if err != nil || !strings.Contains(strings.Join(w.Warnings, "\n"), linked) {
				t.Fatalf("init: %+v %v", w, err)
			}
		})
	}
}
