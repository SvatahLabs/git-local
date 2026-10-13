package restore

import (
	"errors"
	"strings"
	"testing"

	"gitlocal/internal/storage"
)

// GL-119: a restore stopped at a preserved external edit says to move the
// file aside (resume and rollback both refuse while it's there) or abandon,
// rather than only to resume or roll back.
func TestStoppedAtExternalEditNamesTheWayOut(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	err := stopped(id, &externalEdit{path: "notes/c"})
	for _, want := range []string{`new external edit at "notes/c"; preserved`, "move notes/c aside", "recover " + id + " --resume or --rollback", "recover " + id + " --abandon"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message lacks %q: %v", want, err)
		}
	}
	if !errors.Is(err, storage.ErrConflict) || !errors.Is(&externalEdit{path: "a"}, storage.ErrConflict) {
		t.Fatalf("not a conflict: %v", err)
	}
	if other := stopped(id, errors.New("disk full")); strings.Contains(other.Error(), "aside") {
		t.Fatalf("other failures name an edit: %v", other)
	}
}
