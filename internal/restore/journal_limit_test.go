package restore

import (
	"strings"
	"testing"

	"gitlocal/internal/storage"
)

// A journal encode accepts must be readable: exactly journalLimit bytes on
// disk, newline included, is the largest.
func TestJournalSizeLimitAgreesWithReader(t *testing.T) {
	type padded struct{ S string }
	overhead := len(`{"S":""}`) + 1
	dir, err := storage.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	largest, err := encode(padded{strings.Repeat("a", journalLimit-overhead)})
	if err != nil || len(largest) != journalLimit {
		t.Fatalf("journal of exactly the limit: %d bytes, %v", len(largest), err)
	}
	if err := dir.AtomicWrite("journal.json", largest, 0600); err != nil {
		t.Fatal(err)
	}
	if read, err := dir.ReadFile("journal.json", journalLimit); err != nil || len(read) != journalLimit {
		t.Fatalf("read back the largest journal: %d bytes, %v", len(read), err)
	}
	if _, err := encode(padded{strings.Repeat("a", journalLimit-overhead+1)}); err == nil {
		t.Fatal("a journal one byte over the limit was encoded")
	}
}
