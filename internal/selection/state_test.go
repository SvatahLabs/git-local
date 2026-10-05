package selection

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func mustCanonical(t *testing.T, s State) State {
	t.Helper()
	n, err := Canonical(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestPersistentExclusionsAndRetracking(t *testing.T) {
	s, err := Empty().Track(Rule{"notes", "directory"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.Untrack("notes/private.txt")
	if err != nil {
		t.Fatal(err)
	}
	if s.Selected("notes/private.txt") || s.Selected("notes/private.txt/future") || !s.Selected("notes/new.txt") {
		t.Fatal("wrong exclusion scope")
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var loaded State
	if err = json.Unmarshal(b, &loaded); err != nil {
		t.Fatal(err)
	}
	loaded = mustCanonical(t, loaded)
	if !reflect.DeepEqual(s, loaded) {
		t.Fatal("exclusions did not round-trip")
	}
	again, err := s.Track(Rule{"notes", "directory"})
	if err != nil || !reflect.DeepEqual(s, again) {
		t.Fatal("parent tracking cleared exclusions", err)
	}
	again, err = s.Untrack("notes/private.txt")
	if err != nil || !reflect.DeepEqual(s, again) {
		t.Fatal("untrack not idempotent", err)
	}
	restored, err := s.Track(Rule{"notes/private.txt", "file"})
	if err != nil || !restored.Selected("notes/private.txt") || len(restored.Exclude) != 0 {
		t.Fatal("exact retrack failed", err)
	}
	if len(restored.Include) != 1 || restored.Generation != s.Generation+1 {
		t.Fatal("normalization or generation incorrect")
	}
}
func TestExcludedDirectorySemantics(t *testing.T) {
	s, _ := Empty().Track(Rule{"notes", "directory"})
	s, _ = s.Untrack("notes/private")
	if _, err := s.Track(Rule{"notes/private/one", "file"}); err == nil {
		t.Fatal("included below excluded ancestor")
	}
	again, err := s.Untrack("notes/private/one")
	if err != nil || !reflect.DeepEqual(s, again) {
		t.Fatal("nested exclusion not a no-op", err)
	}
	again, err = s.Track(Rule{"notes/private", "directory"})
	if err != nil || !again.Selected("notes/private/one") {
		t.Fatal("directory reactivation failed", err)
	}
	removed, err := s.Untrack("notes")
	if err != nil || len(removed.Include) != 0 || len(removed.Exclude) != 0 {
		t.Fatal("parent removal left orphan rules", err)
	}
}
func TestCanonicalRules(t *testing.T) {
	s := State{SchemaVersion: 2, Include: []Rule{{"notes/a", "file"}, {"notes", "directory"}, {"notes", "directory"}}, Exclude: []string{"notes/private/a", "notes/private"}}
	n := mustCanonical(t, s)
	if !reflect.DeepEqual(n.Include, []Rule{{"notes", "directory"}}) || !reflect.DeepEqual(n.Exclude, []string{"notes/private"}) {
		t.Fatal(n)
	}
	for _, bad := range []State{
		{SchemaVersion: 99},
		{SchemaVersion: 1, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes/private"}},
		{SchemaVersion: 2, Exclude: []string{"orphan"}},
		{SchemaVersion: 2, Include: []Rule{{"notes", "file"}, {"notes", "directory"}}},
		{SchemaVersion: 2, Include: []Rule{{"notes", "file"}, {"notes/child", "file"}}},
		{SchemaVersion: 2, Include: []Rule{{"Notes/a", "file"}, {"notes/b", "file"}}},
		{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"Notes/private"}},
		{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes"}},
		{SchemaVersion: 2, Include: []Rule{{".git/config", "file"}}},
	} {
		if _, err := Canonical(bad); err == nil {
			t.Fatalf("accepted invalid state %#v", bad)
		}
	}
	legacy := mustCanonical(t, State{SchemaVersion: 1, Generation: 8, Include: []Rule{{"notes", "directory"}}})
	if legacy.SchemaVersion != 2 || legacy.Generation != 8 || len(legacy.Exclude) != 0 {
		t.Fatal("bad legacy import")
	}
}
func TestUntrackIsMetadataOnlyAndNormalizesParents(t *testing.T) {
	s := mustCanonical(t, State{SchemaVersion: 2, Include: []Rule{{"a/one", "file"}, {"a/two", "file"}, {"b", "file"}}})
	n, err := s.Untrack("a")
	if err != nil || !reflect.DeepEqual(n.Include, []Rule{{"b", "file"}}) {
		t.Fatal(n, err)
	}
	n2, err := n.Untrack("missing")
	if err != nil || !reflect.DeepEqual(n, n2) {
		t.Fatal("unselected path mutated state")
	}
	if _, err = n.Untrack("B"); err == nil {
		t.Fatal("alias silently ignored")
	}
}
func TestGenerationAndKindChanges(t *testing.T) {
	s := Empty()
	s.Generation = math.MaxUint64
	if _, err := s.Track(Rule{"a", "file"}); err == nil {
		t.Fatal("generation overflow")
	}
	s, _ = Empty().Track(Rule{"a", "file"})
	if _, err := s.Track(Rule{"a", "directory"}); err == nil {
		t.Fatal("silently broadened a file selection")
	}
	before := append([]Rule(nil), s.Include...)
	_, _ = s.Untrack("a")
	if !reflect.DeepEqual(before, s.Include) {
		t.Fatal("input state mutated")
	}
}
