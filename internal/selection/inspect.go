package selection

import (
	"context"
	"gitlocal/internal/storage"
	"sort"
	"strings"
)

// Inspect prunes ownership conflicts before filesystem traversal or content reads.
// Case/Unicode aliases conservatively conflict at the containing include rule.
func (e *Engine) Inspect(ctx context.Context, s State) (Plan, []string, error) {
	n, err := Canonical(s)
	if err != nil {
		return Plan{}, nil, err
	}
	owner, err := InspectOwnership(ctx, e.git, e.root, e.admin)
	if err != nil {
		return Plan{}, nil, err
	}
	conflicts := map[string]bool{}
	for _, p := range owner.Paths {
		key := storage.AliasKey(p)
		for _, r := range n.Include {
			rk := storage.AliasKey(r.Path)
			if within(key, rk) {
				conflicts[r.Path] = true
				continue
			}
			if r.Kind != "directory" || !within(rk, key) {
				continue
			}
			excluded := false
			for _, x := range n.Exclude {
				if within(storage.AliasKey(x), key) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
			// Map an owned descendant only when literal include ancestry agrees.
			if strings.HasPrefix(p, r.Path+"/") {
				conflicts[p] = true
			} else {
				conflicts[r.Path] = true
			}
		}
	}
	paths := []string{}
	for p := range conflicts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	safe := n
	for _, p := range paths {
		safe.Generation = 0
		safe, err = safe.Untrack(p)
		if err != nil {
			return Plan{}, nil, err
		}
	}
	plan, err := e.Validate(ctx, safe)
	if err != nil {
		return Plan{}, nil, err
	}
	if err = owner.Revalidate(ctx, e.git, e.root, e.admin); err != nil {
		return Plan{}, nil, err
	}
	return plan, paths, nil
}
