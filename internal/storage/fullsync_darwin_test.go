//go:build darwin

package storage

import (
	"os"
	"testing"
)

// On Darwin every directory flush goes through F_FULLFSYNC, as file data does
// through os.File.Sync.
func TestDirectoryFlushUsesFullSyncOnDarwin(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := fullSync(int(dir.Fd())); err != nil {
		t.Fatalf("F_FULLFSYNC on a directory: %v", err)
	}
	original := fullSync
	defer func() { fullSync = original }()
	calls := 0
	fullSync = func(fd int) error {
		calls++
		return original(fd)
	}
	d, err := OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	steps := []struct {
		name string
		run  func() error
	}{
		{"mkdir", func() error { return d.MakeDirectory("sub") }},
		{"atomic write", func() error { return d.AtomicWrite("sub/file", []byte("data"), 0600) }},
		{"rename", func() error { return d.Rename("sub/file", "sub/moved") }},
		{"remove", func() error { return d.Remove("sub/moved") }},
		{"remove directory", func() error { return d.RemoveDirectory("sub") }},
	}
	for _, step := range steps {
		before := calls
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if calls == before {
			t.Fatalf("%s flushed its directory without F_FULLFSYNC", step.name)
		}
	}
}
