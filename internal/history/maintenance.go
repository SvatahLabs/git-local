package history

import (
	"context"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/workspace"
)

// MaintenanceResult reports a private store before and after maintenance.
type MaintenanceResult struct {
	Before gitexec.StoreStats `json:"before"`
	After  gitexec.StoreStats `json:"after"`
}

// Maintain packs the private store and deletes objects that no ref reaches:
// leftovers of interrupted or conflicting snapshots and finished restores.
// Every workspace ref, retained workspace and recovery ref keeps its history,
// and quarantined stores live outside the store. It refuses while a
// transaction is pending, since a journal may name objects no ref holds yet.
//
// Packing and pruning hold the exclusive lock; checking the result (a full
// fsck, the longest step on a large store) only reads, so it runs under the
// shared lock, and guards and the overlay filter don't wait for it.
func Maintain(ctx context.Context, r *gitexec.Runner, cwd string) (MaintenanceResult, error) {
	var result MaintenanceResult
	err := configtx.WithStore(ctx, r, cwd, func(w workspace.Context) error {
		var err error
		if result.Before, err = r.StoreStats(ctx, w.Store); err != nil {
			return err
		}
		return r.MaintainStore(ctx, w.Store)
	})
	if err != nil {
		return result, err
	}
	err = configtx.ReadStore(ctx, r, cwd, func(w workspace.Context) error {
		beforeStoreCheck(w)
		if err := r.CheckStore(ctx, w.Store); err != nil {
			return err
		}
		var err error
		result.After, err = r.StoreStats(ctx, w.Store)
		return err
	})
	return result, err
}

// beforeStoreCheck runs before maintenance checks the store; tests replace
// it.
var beforeStoreCheck = func(workspace.Context) {}
