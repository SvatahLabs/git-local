package history

import (
	"context"
	"time"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/workspace"
)

// Private stores run with gc.auto=0, so Git never packs or prunes them on its
// own schedule, concurrently with a transaction. A snapshot packs instead,
// once loose objects pass these limits (Git's gc.auto default is 6,700
// objects; large files reach the size limit first) (GL-136).
var (
	autoPackObjects int64 = 6700
	autoPackBytes   int64 = 256 << 20
	// A store with more packs than this is repacked into one.
	autoPackPacks int64 = 16
	// How long a snapshot waits for the exclusive lock before leaving
	// packing to a later snapshot.
	autoPackLockWait = 5 * time.Second
)

// autoMaintain packs the store after a saved snapshot when it has grown past
// the limits. It never fails the snapshot: a problem is returned as a warning.
func autoMaintain(ctx context.Context, r *gitexec.Runner, cwd string, result *Result) {
	if !result.Changed || result.store == "" {
		return
	}
	count, size, err := gitexec.LooseObjects(result.store)
	if err != nil || count < autoPackObjects && size < autoPackBytes {
		return
	}
	lockCtx, cancel := context.WithTimeout(ctx, autoPackLockWait)
	defer cancel()
	err = configtx.WithStore(lockCtx, r, cwd, func(w workspace.Context) error {
		if err := r.PackLoose(ctx, w.Store); err != nil {
			return err
		}
		stats, err := r.StoreStats(ctx, w.Store)
		if err != nil || stats.Packs <= autoPackPacks {
			return err
		}
		return r.MaintainStore(ctx, w.Store)
	})
	// Waiting too long for the lock, or the command being stopped, leaves
	// packing to a later snapshot.
	if err != nil && ctx.Err() == nil && lockCtx.Err() == nil {
		result.Warnings = append(result.Warnings, "Snapshot saved, but packing the private history store failed ("+err.Error()+"); run git local maintenance.")
	}
}
