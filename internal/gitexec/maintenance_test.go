package gitexec

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// looseStore writes n loose objects of size bytes each, spread over the
// fanout directories, plus entries that aren't objects.
func looseStore(t *testing.T, n, size int) string {
	t.Helper()
	store := t.TempDir()
	objects := filepath.Join(store, "objects")
	for _, dir := range []string{"pack", "info", "zz"} {
		if err := os.MkdirAll(filepath.Join(objects, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := make([]byte, size)
	for i := range n {
		dir := filepath.Join(objects, fmt.Sprintf("%02x", i%7))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%038x", i)), data, 0400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(objects, "00", "tmp_obj_x"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return store
}

// GL-139 review: a threshold check stops counting once a limit is reached,
// so status and log cost at most about the limit's directory entries however
// many loose objects the store holds. The full count is unchanged.
func TestLooseObjectsUpToStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	store := looseStore(t, 600, 100)
	if count, size, err := LooseObjects(store); err != nil || count != 600 || size != 60000 {
		t.Fatalf("LooseObjects = %d, %d, %v; want 600, 60000", count, size, err)
	}
	for _, test := range []struct {
		maxCount, maxBytes, count int64
	}{
		{10, 0, 10},
		{0, 1000, 10},
		{300, 1 << 30, 300},
		{1 << 30, 1050, 11},
		{600, 0, 600},
		{601, 60001, 600},
		{0, 0, 600},
	} {
		count, size, err := LooseObjectsUpTo(store, test.maxCount, test.maxBytes)
		if err != nil || count != test.count || size != 100*test.count {
			t.Fatalf("LooseObjectsUpTo(%d, %d) = %d, %d, %v; want %d visited", test.maxCount, test.maxBytes, count, size, err, test.count)
		}
	}
}
