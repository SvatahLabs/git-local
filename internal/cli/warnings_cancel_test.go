package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlocal/internal/testutil"
)

// TestResetHardPrintsSnapshotWarningsInHumanMode checks that human mode shows
// the warnings JSON mode reports, here for an unavailable workspace.
func TestResetHardPrintsSnapshotWarningsInHumanMode(t *testing.T) {
	repo, write := unavailableWorkspaceFixture(t)
	write(repo.Root, "notes/plain.txt", "plain v2\n")
	code, out, diagnostic := callCLI(t, repo.Root, "reset", "--hard")
	if code != 0 {
		t.Fatalf("reset: exit %d: %s", code, diagnostic)
	}
	if !strings.Contains(out, "Saved local snapshot") || !strings.Contains(out, "Reset main worktree") {
		t.Fatalf("reset output: %q", out)
	}
	if !strings.Contains(diagnostic, "Warning:") || !strings.Contains(diagnostic, "is unavailable") {
		t.Fatalf("reset dropped its warning in human mode: %q", diagnostic)
	}
}

// cancelAfterStart reports no cancellation for its first check, which
// Execute makes before running a command, and is canceled from then on, like
// a Ctrl-C that arrives while the command runs.
type cancelAfterStart struct {
	context.Context
	checks atomic.Int32
	done   chan struct{}
}

func (c *cancelAfterStart) Done() <-chan struct{} {
	if c.checks.Load() == 0 {
		return nil
	}
	return c.done
}

func (c *cancelAfterStart) Err() error {
	if c.checks.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func (c *cancelAfterStart) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterStart) Value(key any) any           { return c.Context.Value(key) }

func TestCancelledGuardExitsAsCanceled(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo.Root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	for _, args := range [][]string{{"guard", "staged"}, {"guard", "push", "origin"}} {
		done := make(chan struct{})
		close(done)
		var out, diagnostic bytes.Buffer
		code := Execute(&cancelAfterStart{Context: context.Background(), done: done}, append([]string{"--json"}, args...), &out, &diagnostic)
		var envelope struct {
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || code != 1 || envelope.Error == nil || envelope.Error.Code != "canceled" {
			t.Fatalf("%v: exit %d, %s (%v)", args, code, out.String(), err)
		}
	}
}
