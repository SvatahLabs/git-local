package selection

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitlocal/internal/storage"
)

func TestOwnershipParserAndAliasCollisions(t *testing.T) {
	oid := strings.Repeat("a", 40)
	good := []byte("100644 " + oid + " 2\tname\twith-tab\x00")
	entries, err := parseEntries(good, true)
	if err != nil || len(entries) != 1 || entries[0].path != "name\twith-tab" {
		t.Fatal("tab filename parsed incorrectly", err)
	}
	for _, bad := range []string{"100644 " + oid + " 0\ta", "broken\x00", "100644 " + oid + " 4\ta\x00", "100644 blob bad-id\ta\x00", "040000 " + oid + " 0\t../escape/\x00"} {
		if _, err := parseEntries([]byte(bad), true); err == nil {
			t.Fatalf("accepted malformed record %q", bad)
		}
	}
	cases := []struct {
		owned    string
		s        State
		conflict bool
	}{
		{"NOTES/private", State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}}, true},
		{"café/file", State{SchemaVersion: 2, Include: []Rule{{"cafe\u0301", "directory"}}}, true},
		{"notes", State{SchemaVersion: 2, Include: []Rule{{"notes/child", "file"}}}, true},
		{"notes/private", State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes/private"}}, false},
		{"notes/shared", State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes/private"}}, true},
	}
	for _, tc := range cases {
		err := (Ownership{Paths: []string{tc.owned}}).Check(tc.s)
		if errors.Is(err, storage.ErrConflict) != tc.conflict {
			t.Fatalf("%q: %v", tc.owned, err)
		}
	}
}

type unavailableGit struct{}

func (unavailableGit) Main(context.Context, string, string, ...string) ([]byte, error) {
	return nil, errors.New("ownership unavailable")
}
func TestUnavailableOwnershipIsNotEmpty(t *testing.T) {
	if _, err := InspectOwnership(context.Background(), unavailableGit{}, "", " "); err == nil {
		t.Fatal("unavailable ownership accepted")
	}
}
