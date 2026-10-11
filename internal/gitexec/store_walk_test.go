package gitexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// Every history command validates the store, but the walk over every file
// in it runs once per runner; the config and redirection files are still
// checked on every command.
func TestStoreIsWalkedOncePerRunner(t *testing.T) {
	repo := testutil.New(t)
	r := newRunner(t, repo)
	ctx := context.Background()
	store := filepath.Join(t.TempDir(), "history.git")
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	// The first command also probes Git's capabilities in a store of its own.
	if _, err := r.History(ctx, store, nil, "rev-parse", "--git-dir"); err != nil {
		t.Fatal(err)
	}
	before := storeWalks.Load()
	for i := 0; i < 50; i++ {
		if _, err := r.History(ctx, store, strings.NewReader("blob\n"), "hash-object", "-w", "--stdin"); err != nil {
			t.Fatal(err)
		}
	}
	if walks := storeWalks.Load() - before; walks != 0 {
		t.Fatalf("50 history commands walked the store %d times", walks)
	}
	if err := os.WriteFile(filepath.Join(store, "objects", "info", "alternates"), []byte("/elsewhere\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.History(ctx, store, nil, "rev-parse", "--git-dir"); err == nil || !strings.Contains(err.Error(), "alternates") {
		t.Fatalf("redirection after the walk accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(store, "objects", "info", "alternates")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "config"), []byte(StoreConfig+"[core]\n\thooksPath = /tmp\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.History(ctx, store, nil, "rev-parse", "--git-dir"); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("changed config after the walk accepted: %v", err)
	}
	// A new runner (the next command) walks again.
	other := newRunner(t, repo)
	if _, err := other.History(ctx, t.TempDir(), nil, "rev-parse"); err == nil {
		t.Fatal("history command outside a store accepted")
	}
	if err := os.WriteFile(filepath.Join(store, "config"), []byte(StoreConfig), 0600); err != nil {
		t.Fatal(err)
	}
	before = storeWalks.Load()
	if _, err := other.History(ctx, store, nil, "rev-parse", "--git-dir"); err != nil {
		t.Fatal(err)
	}
	if storeWalks.Load()-before != 1 {
		t.Fatal("a new runner didn't walk the store")
	}
}
