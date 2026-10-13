package configtx

import (
	"errors"
	"strings"
	"testing"
)

// A journal is first written with room for its later direction and outcome
// writes: one that fits only without that room is refused before anything
// changes, while a later write may use the room.
func TestJournalFirstWriteKeepsHeadroom(t *testing.T) {
	type journal struct {
		Data    string `json:"data"`
		Outcome string `json:"outcome,omitempty"`
	}
	j := journal{Data: strings.Repeat("x", JournalLimit-journalHeadroom/2)}
	if _, err := encodeNew(j); !errors.Is(err, errJournalLimit) {
		t.Fatalf("first write without headroom: %v", err)
	}
	j.Outcome = "rolledback"
	if _, err := encode(j); err != nil {
		t.Fatalf("later write within the limit: %v", err)
	}
	j = journal{Data: strings.Repeat("x", JournalLimit-2*journalHeadroom)}
	if _, err := encodeNew(j); err != nil {
		t.Fatalf("first write with headroom: %v", err)
	}
}
