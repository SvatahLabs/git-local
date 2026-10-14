package selection

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// canonicalPairwise is Canonical as it was before GL-122, comparing every
// pair of rules; the new one must decide exactly as it did.
func canonicalPairwise(s State) (State, error) {
	if s.SchemaVersion != 1 && s.SchemaVersion != SchemaVersion {
		return State{}, errors.New("unsupported selection schema")
	}
	if s.SchemaVersion == 1 && len(s.Exclude) > 0 {
		return State{}, errors.New("exclusions require selection schema 2")
	}
	n := Empty()
	n.Generation = s.Generation
	n.Include = append(n.Include, s.Include...)
	n.Exclude = append(n.Exclude, s.Exclude...)
	aliases := map[string]string{}
	for _, r := range n.Include {
		if r.Kind != "file" && r.Kind != "directory" {
			return State{}, fmt.Errorf("invalid selection kind for %q", r.Path)
		}
		if err := validatePath(r.Path, aliases); err != nil {
			return State{}, err
		}
	}
	for _, p := range n.Exclude {
		if err := validatePath(p, aliases); err != nil {
			return State{}, err
		}
	}
	sort.Slice(n.Include, func(i, j int) bool { return n.Include[i].Path < n.Include[j].Path })
	includes := []Rule{}
	for _, r := range n.Include {
		redundant := false
		for _, a := range includes {
			if a.Path == r.Path && a.Kind != r.Kind {
				return State{}, fmt.Errorf("conflicting selection kinds for %q", r.Path)
			}
			if covers(a, r.Path) {
				redundant = true
				break
			}
			if within(a.Path, r.Path) {
				return State{}, fmt.Errorf("file selection %q cannot have selected descendants", a.Path)
			}
		}
		if !redundant {
			includes = append(includes, r)
		}
	}
	n.Include = includes
	sort.Strings(n.Exclude)
	excludes := []string{}
	for _, p := range n.Exclude {
		covered := false
		for _, r := range n.Include {
			if r.Kind == "directory" && r.Path != p && within(r.Path, p) {
				covered = true
			}
			if within(p, r.Path) {
				return State{}, fmt.Errorf("exclusion %q obscures an explicit include", p)
			}
		}
		if !covered {
			return State{}, fmt.Errorf("exclusion %q has no containing directory selection", p)
		}
		redundant := false
		for _, a := range excludes {
			if within(a, p) {
				redundant = true
				break
			}
		}
		if !redundant {
			excludes = append(excludes, p)
		}
	}
	n.Exclude = excludes
	return n, nil
}

// Canonical decides every state as the pairwise version did: the same
// result, or the same error. The paths share prefixes and sort-order traps
// ("a-b" sorts between "a" and "a/b").
func TestCanonicalMatchesPairwiseVersion(t *testing.T) {
	paths := []string{"a", "a/b", "a/b/c", "a-b", "a-b/c", "a.b", "a/b-c", "a/b/c/d", "b", "b/a", "c"}
	kinds := []string{"file", "directory"}
	random := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		s := Empty()
		for n := random.Intn(5); n > 0; n-- {
			s.Include = append(s.Include, Rule{paths[random.Intn(len(paths))], kinds[random.Intn(2)]})
		}
		for n := random.Intn(3); n > 0; n-- {
			s.Exclude = append(s.Exclude, paths[random.Intn(len(paths))])
		}
		got, gotErr := Canonical(s)
		want, wantErr := canonicalPairwise(s)
		if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v:\ngot  %+v %v\nwant %+v %v", s, got, gotErr, want, wantErr)
		}
	}
}

// Canonical's work grows with K log K for K rules, not K² (GL-122). Counted
// by allocations: the pairwise version allocated a prefix for each pair of
// rules it compared (about 4.5 million for 3,000 rules); the paths are
// longer than the 32 bytes Go builds such a prefix in on the stack.
func TestCanonicalDoesntCompareEveryPairOfRules(t *testing.T) {
	const k = 3000
	s := Empty()
	for i := 0; i < k; i++ {
		s.Include = append(s.Include, Rule{fmt.Sprintf("a-directory-with-a-long-name-%d/file-%05d", i%7, i), "file"})
	}
	for i := 0; i < 7; i++ {
		s.Include = append(s.Include, Rule{fmt.Sprintf("a-directory-with-a-long-name-%d/sub", i), "directory"})
		s.Exclude = append(s.Exclude, fmt.Sprintf("a-directory-with-a-long-name-%d/sub/private", i))
	}
	var kept int
	allocations := testing.AllocsPerRun(1, func() {
		n, err := Canonical(s)
		if err != nil {
			t.Fatal(err)
		}
		kept = len(n.Include) + len(n.Exclude)
	})
	t.Logf("canonicalizing %d rules: %.0f allocations", k, allocations)
	if kept != k+14 || allocations > 40*k {
		t.Fatalf("canonicalizing %d rules kept %d and made %.0f allocations", k, kept, allocations)
	}
}
