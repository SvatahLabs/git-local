package automatic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// watchStep bounds each wait for the watcher, not the whole test: under -race
// and a loaded package run one snapshot can take tens of seconds.
const watchStep = 3 * time.Minute

func TestWatchCapturesChangesAndStopsCleanly(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	setup := context.Background()
	if _, err := workspace.Init(setup, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo.Root, "notes.local")
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(setup, r, repo.Root, true, "notes.local"); err != nil {
		t.Fatal(err)
	}
	beforeIndex := repo.Git("ls-files", "--stage", "-z")
	beforeHead := repo.Git("rev-parse", "HEAD")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	captured := make(chan history.Result, 2)
	conflicts := make(chan error, MaxWatchConflicts)
	done := make(chan error, 1)
	go func() {
		done <- watchTicks(ctx, r, repo.Root, ticks, func(result history.Result) error {
			captured <- result
			return nil
		}, func(err error) { conflicts <- err })
	}()
	// A capture conflict waits for the next tick; send one instead of waiting
	// for a timer that the test doesn't run.
	wait := func(what string) history.Result {
		t.Helper()
		deadline := time.After(watchStep)
		for {
			select {
			case result := <-captured:
				return result
			case err := <-conflicts:
				t.Logf("%s: capture conflict, retrying: %v", what, err)
				ticks <- time.Now()
			case err := <-done:
				t.Fatalf("watch stopped before the %s: %v", what, err)
			case <-deadline:
				t.Fatalf("the %s took longer than %v", what, watchStep)
			}
		}
	}
	first := wait("initial snapshot")
	if err := os.WriteFile(path, []byte("second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Now()
	second := wait("changed snapshot")
	if first.ID == second.ID || !first.Changed || !second.Changed {
		t.Fatalf("timer failed to save both versions: %+v %+v", first, second)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch did not stop cleanly: %v", err)
		}
	case <-time.After(watchStep):
		t.Fatalf("watch didn't stop within %v of cancellation", watchStep)
	}
	entries, err := history.Log(context.Background(), r, repo.Root, 10)
	if err != nil || len(entries) != 2 || entries[0].ID != second.ID || entries[1].ID != first.ID {
		t.Fatalf("timer history is wrong: %+v %v", entries, err)
	}
	if got := repo.Git("ls-files", "--stage", "-z"); !bytes.Equal(got, beforeIndex) {
		t.Fatal("watch changed the main index")
	}
	if got := repo.Git("rev-parse", "HEAD"); !bytes.Equal(got, beforeHead) {
		t.Fatal("watch advanced main HEAD")
	}
}

func TestWatchReportsPublishedSnapshotOnCancellation(t *testing.T) {
	result := history.Result{ID: "published", Changed: true}
	var reported history.Result
	err := reportSnapshot(result, context.Canceled, func(got history.Result) error {
		reported = got
		return nil
	})
	if !errors.Is(err, context.Canceled) || reported.ID != result.ID {
		t.Fatalf("published snapshot was lost on cancellation: %+v %v", reported, err)
	}
	reportFailure := errors.New("output failed")
	err = reportSnapshot(result, context.Canceled, func(history.Result) error { return reportFailure })
	if !errors.Is(err, reportFailure) || errors.Is(err, context.Canceled) {
		t.Fatalf("output failure was hidden by cancellation: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if canceledCapture(ctx, fmt.Errorf("%w: %w", storage.ErrConflict, context.Canceled)) {
		t.Fatal("publication conflict was hidden by cancellation")
	}
}
