//go:build darwin || linux

package selection

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSpecialFilesAndUnreadableDirectoriesFail(t *testing.T) {
	r, e := fixture(t)
	write(t, r, "notes/plain")
	if err := unix.Mkfifo(filepath.Join(r.Root, "notes/pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanTrack(context.Background(), Empty(), r.Root, "notes"); err == nil {
		t.Fatal("selected FIFO")
	}
	if err := os.Remove(filepath.Join(r.Root, "notes/pipe")); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory permission checks")
	}
	path := filepath.Join(r.Root, "notes")
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0700)
	s := mustCanonical(t, State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}})
	if _, err := e.Validate(context.Background(), s); err == nil {
		t.Fatal("unreadable directory treated as deleted or empty")
	}
}
