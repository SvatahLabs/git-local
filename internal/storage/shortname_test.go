package storage

import (
	"runtime"
	"testing"
)

// TestSelectionPathRejectsShortNamesOnWindows: on NTFS with 8.3 names, a
// component like PROGRA~1 resolves to a long name that alias, ownership and
// restore-path checks can't see, so Windows refuses such components. Elsewhere
// they are ordinary names (editor backups like backup~2.txt) and stay valid,
// so existing selections and snapshots keep loading.
func TestSelectionPathRejectsShortNamesOnWindows(t *testing.T) {
	for _, path := range []string{"PROGRA~1", "notes/NAME~1.EXT", "notes/name~12.txt", "LONGFI~3/secret", "a~1", "notes/backup~2.txt"} {
		err := ValidateSelectionPath(path)
		if runtime.GOOS == "windows" && err == nil {
			t.Errorf("%q was accepted on Windows", path)
		}
		if runtime.GOOS != "windows" && err != nil {
			t.Errorf("%q was refused on %s: %v", path, runtime.GOOS, err)
		}
	}
	for _, path := range []string{"notes/backup~", "notes/a~b.txt", "notes/x~1.tar.gz", "notes/~1x", "notes/plain.txt"} {
		if err := ValidateSelectionPath(path); err != nil {
			t.Errorf("%q was refused: %v", path, err)
		}
	}
}
