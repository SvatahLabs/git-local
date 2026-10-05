package automatic

import (
	"context"
	"errors"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/storage"
)

const MinWatchInterval = time.Second
const MaxWatchInterval = 24 * time.Hour

// Watch keeps one checkout's selected files captured while its process runs.
// It takes an initial snapshot, then checks on each tick. The normal snapshot
// lock makes this safe alongside Git event hooks and other writers.
func Watch(ctx context.Context, r *gitexec.Runner, cwd string, interval time.Duration, onSnapshot func(history.Result) error) error {
	if interval < MinWatchInterval || interval > MaxWatchInterval {
		return errors.New("watch interval must be between 1s and 24h")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	return watchTicks(ctx, r, cwd, ticker.C, onSnapshot)
}

func watchTicks(ctx context.Context, r *gitexec.Runner, cwd string, ticks <-chan time.Time, onSnapshot func(history.Result) error) error {
	capture := func() error {
		result, err := history.Commit(ctx, r, cwd, "Automatic local snapshot on timer")
		return reportSnapshot(result, err, onSnapshot)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := capture(); err != nil {
		if canceledCapture(ctx, err) {
			return nil
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return errors.New("watch timer stopped unexpectedly")
			}
			if err := capture(); err != nil {
				if canceledCapture(ctx, err) {
					return nil
				}
				return err
			}
		}
	}
}

func reportSnapshot(result history.Result, commitErr error, onSnapshot func(history.Result) error) error {
	if result.Changed && onSnapshot != nil {
		if reportErr := onSnapshot(result); reportErr != nil {
			if commitErr != nil && !errors.Is(commitErr, context.Canceled) && !errors.Is(commitErr, context.DeadlineExceeded) {
				return errors.Join(commitErr, reportErr)
			}
			return reportErr
		}
	}
	return commitErr
}

func canceledCapture(ctx context.Context, err error) bool {
	return ctx.Err() != nil && !errors.Is(err, storage.ErrConflict) &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
