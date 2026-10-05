// Package selection models literal includes and exclusions and validates their
// effective scope. It never publishes configuration; T06 owns transactions.
package selection

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"gitlocal/internal/storage"
)

const SchemaVersion = 2

type Rule struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}
type State struct {
	SchemaVersion int    `json:"schemaVersion"`
	Generation    uint64 `json:"generation"`
	Include       []Rule `json:"include"`
	// Exclusions are literal path subtrees, independent of current filesystem type.
	Exclude []string `json:"exclude"`
}

func Empty() State {
	return State{SchemaVersion: SchemaVersion, Include: []Rule{}, Exclude: []string{}}
}
func within(parent, path string) bool { return path == parent || strings.HasPrefix(path, parent+"/") }
func covers(rule Rule, path string) bool {
	return rule.Path == path || rule.Kind == "directory" && within(rule.Path, path)
}

// Canonical returns an independent, sorted, normalized state. Version 1 is read
// compatibly in memory; no on-disk migration happens here. Exclusions require v2
// so older readers fail closed instead of silently capturing excluded contents.
func Canonical(s State) (State, error) {
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
func validatePath(path string, aliases map[string]string) error {
	if err := storage.ValidateSelectionPath(path); err != nil {
		return err
	}
	// Rules persist canonical slash paths even on Windows.
	if strings.Contains(path, "\\") {
		return errors.New("selection paths must use slash separators")
	}
	parts := strings.Split(path, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		key := storage.AliasKey(p)
		if old, ok := aliases[key]; ok && old != p {
			return fmt.Errorf("ambiguous path aliases %q and %q", old, p)
		}
		aliases[key] = p
	}
	return nil
}

// Excluded and Selected expect a canonical state and canonical project path.
func (s State) Excluded(path string) bool {
	for _, p := range s.Exclude {
		if within(p, path) {
			return true
		}
	}
	return false
}
func (s State) Selected(path string) bool {
	if s.Excluded(path) {
		return false
	}
	for _, r := range s.Include {
		if covers(r, path) {
			return true
		}
	}
	return false
}
func (s State) SemanticEqual(other State) bool {
	return reflect.DeepEqual(s.Include, other.Include) && reflect.DeepEqual(s.Exclude, other.Exclude)
}
func changed(old, next State) (State, error) {
	n, err := Canonical(next)
	if err != nil {
		return State{}, err
	}
	if !n.SemanticEqual(old) {
		if old.Generation == math.MaxUint64 {
			return State{}, errors.New("selection generation exhausted")
		}
		n.Generation = old.Generation + 1
	}
	return n, nil
}

// Track explicitly reactivates an exact excluded path, but never removes child
// exclusions when an already-selected parent is tracked again. Re-inclusion below
// an excluded ancestor is intentionally rejected; reactivate that ancestor first.
func (s State) Track(rule Rule) (State, error) {
	old, err := Canonical(s)
	if err != nil {
		return State{}, err
	}
	n, err := Canonical(old)
	if err != nil {
		return State{}, err
	}
	if err = storage.ValidateSelectionPath(rule.Path); err != nil {
		return State{}, err
	}
	for _, r := range old.Include {
		if r.Path == rule.Path && r.Kind != rule.Kind {
			return State{}, fmt.Errorf("selection kind changed for %q; untrack it before re-tracking", rule.Path)
		}
	}
	remaining := []string{}
	for _, p := range n.Exclude {
		if p != rule.Path {
			if within(p, rule.Path) {
				return State{}, fmt.Errorf("%w: %q is excluded by %q; re-track that excluded ancestor first", storage.ErrConflict, rule.Path, p)
			}
			remaining = append(remaining, p)
		}
	}
	n.Exclude = remaining
	n.Include = append(n.Include, rule)
	return changed(old, n)
}

// Untrack is entirely metadata-only, including for missing or newly main-owned
// paths. Inside an included directory it records a persistent subtree exclusion.
func (s State) Untrack(path string) (State, error) {
	old, err := Canonical(s)
	if err != nil {
		return State{}, err
	}
	if err = storage.ValidateSelectionPath(path); err != nil {
		return State{}, err
	}
	aliases := map[string]string{}
	for _, r := range old.Include {
		if err = validatePath(r.Path, aliases); err != nil {
			return State{}, err
		}
	}
	for _, p := range old.Exclude {
		if err = validatePath(p, aliases); err != nil {
			return State{}, err
		}
	}
	if err = validatePath(path, aliases); err != nil {
		return State{}, err
	}
	if old.Excluded(path) {
		return old, nil
	}
	n := Empty()
	n.Generation = old.Generation
	for _, r := range old.Include {
		if !within(path, r.Path) {
			n.Include = append(n.Include, r)
		}
	}
	for _, p := range old.Exclude {
		if !within(path, p) {
			n.Exclude = append(n.Exclude, p)
		}
	}
	for _, r := range n.Include {
		if r.Kind == "directory" && within(r.Path, path) {
			n.Exclude = append(n.Exclude, path)
			break
		}
	}
	return changed(old, n)
}
