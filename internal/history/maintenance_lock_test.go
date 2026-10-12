package history

import (
	"context"
	"testing"
	"time"

	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// GL-115: maintenance checks the store (fsck, its longest step on a large
// store) under the shared lock, so guards and the overlay filter, which take
// the shared lock, don't wait for it; the check still passes. Not parallel:
// it replaces beforeStoreCheck.
func TestMaintenanceChecksStoreUnderSharedLock(t *testing.T) {
	r, g, _ := fixture(t)
	write(t, r.Root, "notes/a", []byte("a"))
	change(t, g, r.Root, true, "notes")
	save(t, g, r.Root)
	shared := false
	beforeStoreCheck = func(w workspace.Context) {
		common, err := storage.OpenDir(w.Common)
		if err != nil {
			t.Error(err)
			return
		}
		defer common.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
		if err == nil {
			shared = true
			lock.Close()
		}
	}
	t.Cleanup(func() { beforeStoreCheck = func(workspace.Context) {} })
	result, err := Maintain(context.Background(), g, r.Root)
	if err != nil || result.After.PackedObjects == 0 {
		t.Fatalf("maintenance: %+v %v", result, err)
	}
	if !shared {
		t.Fatal("a guard couldn't take the shared lock while maintenance checked the store")
	}
}
