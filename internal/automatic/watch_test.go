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

func TestWatchCapturesChangesAndStopsCleanly(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo.Root, "notes.local")
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "notes.local"); err != nil {
		t.Fatal(err)
	}
	beforeIndex := repo.Git("ls-files", "--stage", "-z")
	beforeHead := repo.Git("rev-parse", "HEAD")
	ticks := make(chan time.Time, 1)
	captured := make(chan history.Result, 2)
	done := make(chan error, 1)
	go func() {
		done <- watchTicks(ctx, r, repo.Root, ticks, func(result history.Result) error {
			captured <- result
			return nil
		})
	}()
	var first, second history.Result
	select {
	case first = <-captured:
	case err := <-done:
		t.Fatalf("watch stopped before initial snapshot: %v", err)
	case <-ctx.Done():
		t.Fatal("initial snapshot timed out")
	}
	if err := os.WriteFile(path, []byte("second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Now()
	select {
	case second = <-captured:
	case err := <-done:
		t.Fatalf("watch stopped before changed snapshot: %v", err)
	case <-ctx.Done():
		t.Fatal("changed snapshot timed out")
	}
	if first.ID == second.ID || !first.Changed || !second.Changed {
		t.Fatalf("timer failed to save both versions: %+v %+v", first, second)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watch did not stop cleanly: %v", err)
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
