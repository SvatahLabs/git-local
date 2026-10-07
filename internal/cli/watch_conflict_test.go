package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/testutil"
)

// lockedBuffer lets a test read output that a running command still writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startWatch runs auto watch with a one-second interval in a repository
// selecting notes.local, and waits for its initial snapshot.
func startWatch(t *testing.T) (*testutil.Repo, *gitexec.Runner, *lockedBuffer, context.CancelFunc, chan int) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo.Root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(original) })
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out, diagnostic := new(lockedBuffer), new(lockedBuffer)
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"auto", "watch", "--interval", "1s"}, out, diagnostic)
	}()
	waitForSnapshots(t, r, repo, done, diagnostic, 1)
	return repo, r, diagnostic, cancel, done
}

func waitForSnapshots(t *testing.T, r *gitexec.Runner, repo *testutil.Repo, done chan int, diagnostic *lockedBuffer, want int) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		entries, err := history.Log(context.Background(), r, repo.Root, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) >= want {
			return
		}
		select {
		case code := <-done:
			t.Fatalf("watch exited with %d before snapshot %d: %s", code, want, diagnostic.String())
		case <-deadline:
			t.Fatalf("snapshot %d not captured: %s", want, diagnostic.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestWatchContinuesAfterTransientConflict stages a selected file, which makes
// the next captures conflict, then unstages it: the watcher keeps running,
// warns, and captures the following change.
func TestWatchContinuesAfterTransientConflict(t *testing.T) {
	repo, r, diagnostic, cancel, done := startWatch(t)
	repo.Git("add", "-f", "notes.local")
	deadline := time.After(30 * time.Second)
	for !strings.Contains(diagnostic.String(), "Warning:") {
		select {
		case code := <-done:
			t.Fatalf("watch exited with %d on a transient conflict: %s", code, diagnostic.String())
		case <-deadline:
			t.Fatalf("no conflict warning: %s", diagnostic.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	repo.Git("rm", "-q", "--cached", "notes.local")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	waitForSnapshots(t, r, repo, done, diagnostic, 2)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("watch stop: exit %d: %s", code, diagnostic.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not stop after cancellation")
	}
	data, err := history.Show(context.Background(), r, repo.Root, "", "latest", "notes.local")
	if err != nil || string(data) != "second\n" {
		t.Fatalf("latest snapshot holds %q, %v", data, err)
	}
	if !strings.Contains(diagnostic.String(), "retrying") {
		t.Fatalf("warning doesn't say the watcher retries: %s", diagnostic.String())
	}
}

// TestWatchExitsAfterConsecutiveConflicts keeps the conflict: the watcher
// warns for the first attempts and exits 3 on the fifth.
func TestWatchExitsAfterConsecutiveConflicts(t *testing.T) {
	repo, _, diagnostic, _, done := startWatch(t)
	repo.Git("add", "-f", "notes.local")
	select {
	case code := <-done:
		if code != 3 {
			t.Fatalf("exit %d, want 3: %s", code, diagnostic.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("watch kept running through repeated conflicts: %s", diagnostic.String())
	}
	if got := strings.Count(diagnostic.String(), "Warning:"); got != 4 {
		t.Fatalf("got %d conflict warnings, want 4: %s", got, diagnostic.String())
	}
	if !strings.Contains(diagnostic.String(), "5 snapshot attempts in a row failed") {
		t.Fatalf("final error doesn't explain the exit: %s", diagnostic.String())
	}
}
