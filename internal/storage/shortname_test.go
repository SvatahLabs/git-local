package storage

import "testing"

// TestSelectionPathRejectsShortNames: on NTFS with 8.3 names, a component like
// PROGRA~1 resolves to a long name that alias, ownership and restore-path
// checks can't see, so such components are refused on every platform.
func TestSelectionPathRejectsShortNames(t *testing.T) {
	for _, path := range []string{"PROGRA~1", "notes/NAME~1.EXT", "notes/name~12.txt", "LONGFI~3/secret", "a~1"} {
		if err := ValidateSelectionPath(path); err == nil {
			t.Errorf("%q was accepted", path)
		}
	}
	for _, path := range []string{"notes/backup~", "notes/a~b.txt", "notes/x~1.tar.gz", "notes/~1x", "notes/plain.txt"} {
		if err := ValidateSelectionPath(path); err != nil {
			t.Errorf("%q was refused: %v", path, err)
		}
	}
}
