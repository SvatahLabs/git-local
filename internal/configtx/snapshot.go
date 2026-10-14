package configtx

import (
	"context"
	"fmt"
	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// WithSelection holds the common mutation lock through capture and publication.
// recheck detects edits to every configuration input and verifies current ignores.
func WithSelection(ctx context.Context, r *gitexec.Runner, cwd string, fn func(workspace.Context, selection.State, func() error) error) ([]string, error) {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, s.pendingError(pending[0])
	}
	var state selection.State
	for _, src := range s.sources {
		if src.ID == s.w.WorkspaceID {
			state, err = src.state()
			if err != nil {
				return nil, err
			}
		}
	}
	before, err := readImage(s.common, "info/exclude")
	if err != nil {
		return nil, err
	}
	all, err := states(s.sources, "", nil)
	if err != nil {
		return nil, err
	}
	patterns, alternates, err := s.compile(all)
	if err != nil {
		return nil, err
	}
	if _, err = exclude.UserBytes(before.Data, patterns, alternates...); err != nil {
		return nil, err
	}
	warnings, err := s.verify(ctx, s.w.WorkspaceID, state, false)
	if err != nil {
		return warnings, err
	}
	recheck := func() error {
		actual, e := s.readSources()
		if e != nil {
			return e
		}
		if len(actual) != len(s.sources) {
			return fmt.Errorf("%w: configuration changed during capture", storage.ErrConflict)
		}
		for i, src := range actual {
			old := s.sources[i]
			if src.ID != old.ID || !same(src.Record, old.Record) || !same(src.Selection, old.Selection) {
				return fmt.Errorf("%w: configuration changed during capture", storage.ErrConflict)
			}
		}
		now, e := readImage(s.common, "info/exclude")
		if e != nil {
			return e
		}
		if !same(now, before) {
			return fmt.Errorf("%w: ignore rules changed during capture", storage.ErrConflict)
		}
		pending, e := s.pending()
		if e != nil {
			return e
		}
		if len(pending) > 0 {
			return s.pendingError(pending[0])
		}
		more, e := s.verify(ctx, s.w.WorkspaceID, state, false)
		warnings = append(warnings, more...)
		return e
	}
	err = fn(*s.w, state, recheck)
	return uniqueWarnings(warnings), err
}
