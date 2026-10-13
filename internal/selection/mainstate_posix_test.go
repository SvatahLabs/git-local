//go:build darwin || linux

package selection

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRacyTimestamps(t *testing.T) {
	hashed := time.Date(2026, 10, 11, 12, 0, 10, 500, time.UTC)
	for _, c := range []struct {
		mtime time.Time
		racy  bool
	}{
		{hashed.Add(-time.Millisecond), false},
		{hashed, true},
		{hashed.Add(time.Second), true},
		{time.Date(2026, 10, 11, 12, 0, 9, 0, time.UTC), true},  // whole second within 2 s
		{time.Date(2026, 10, 11, 12, 0, 8, 0, time.UTC), false}, // whole second, 2 s before
	} {
		if got := Racy(fakeInfo{c.mtime}, hashed); got != c.racy {
			t.Errorf("mtime %v: racy %v, want %v", c.mtime, got, c.racy)
		}
	}
}

type fakeInfo struct{ mtime time.Time }

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return 0644 }
func (f fakeInfo) ModTime() time.Time { return f.mtime }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

// Git replaces the target of a symlinked index; the cheap global state must
// see that, and compare change times (not only identity, size and time).
func TestIndexStatFollowsSymlinkAndComparesChangeTime(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "real-index"), filepath.Join(dir, "index")
	if err := os.WriteFile(target, []byte("index v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	before, err := StatIndex(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.Sys().(*unix.Stat_t); !ok || ChangeTime(before).IsZero() {
		t.Fatalf("index stat has no change time: %T", before.Sys())
	}
	// Replace the target as Git does: write a new file and rename it over.
	if err := os.WriteFile(target+".lock", []byte("index v2"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target+".lock", target); err != nil {
		t.Fatal(err)
	}
	after, err := StatIndex(link)
	if err != nil {
		t.Fatal(err)
	}
	if (MainState{Index: before}).Same(MainState{Index: after}) {
		t.Fatal("a replaced index behind a symlink looks unchanged")
	}
}
