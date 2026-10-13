package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GL-118: a command waiting for the repository lock while git local deinit
// renames the private state is told what happened, instead of going on to
// fail on the first missing file.
func TestLockReportsStateMovedWhileWaiting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "gitlocal", "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const name = "gitlocal/locks/repository.lock"
	held, err := d.Lock(ctx, name, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, shared := range []bool{false, true} {
		done := make(chan error, 1)
		go func() {
			var l *Lock
			var err error
			if shared {
				l, err = d.LockExisting(ctx, name)
			} else {
				l, err = d.Lock(ctx, name, false)
			}
			if l != nil {
				l.Close()
			}
			done <- err
		}()
		// Give the waiter time to open the lock file and start waiting.
		time.Sleep(300 * time.Millisecond)
		if err := os.Rename(filepath.Join(root, "gitlocal"), filepath.Join(root, "gitlocal-removed")); err != nil {
			t.Fatal(err)
		}
		held.Close()
		err := <-done
		if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "removed or replaced while this command waited") {
			t.Fatalf("waiter (shared %v) after the state moved: %v", shared, err)
		}
		if err := os.Rename(filepath.Join(root, "gitlocal-removed"), filepath.Join(root, "gitlocal")); err != nil {
			t.Fatal(err)
		}
		if held, err = d.Lock(ctx, name, false); err != nil {
			t.Fatal(err)
		}
	}
	held.Close()
}
