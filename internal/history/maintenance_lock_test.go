package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
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

// Between packing and checking the store, another command may start (or
// leave interrupted) a transaction; maintenance already did its work and
// still reports success. Not parallel: it replaces afterPacking.
func TestMaintenanceSucceedsWhenATransactionStartsBeforeItsCheck(t *testing.T) {
	r, g, _ := fixture(t)
	write(t, r.Root, "notes/a", []byte("a"))
	change(t, g, r.Root, true, "notes")
	save(t, g, r.Root)
	afterPacking = func(w workspace.Context) {
		id, err := storage.UUID()
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(w.Common, "gitlocal", "transactions", "config-"+id+".json"), []byte("{}"), 0600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterPacking = func(workspace.Context) {} })
	if result, err := Maintain(context.Background(), g, r.Root); err != nil || result.After.PackedObjects == 0 {
		t.Fatalf("maintenance with a transaction started before its check: %+v %v", result, err)
	}
}
