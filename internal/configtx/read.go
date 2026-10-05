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

// ReadSelection does not enforce current file ownership: read commands report
// conflicts instead. It neither creates a missing lock nor repairs metadata.
func ReadSelection(ctx context.Context, r *gitexec.Runner, cwd string, fn func(workspace.Context, selection.State, []string, func() error) error) error {
	s, err := openMode(ctx, r, cwd, true)
	if err != nil {
		return err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return pendingError(pending[0])
	}
	var state selection.State
	for _, src := range s.sources {
		if src.ID == s.w.WorkspaceID {
			state, err = src.state()
			if err != nil {
				return err
			}
		}
	}
	before, err := readImage(s.common, "info/exclude")
	if err != nil {
		return err
	}
	warnings := []string{}
	all, err := states(s.sources, "", nil)
	if err != nil {
		return err
	}
	patterns, err := exclude.Compile(all)
	if err != nil {
		return err
	}
	if _, err = exclude.UserBytes(before.Data, patterns); err != nil {
		warnings = append(warnings, err.Error())
	}
	recheck := func() error {
		actual, e := s.readSources()
		if e != nil {
			return e
		}
		if len(actual) != len(s.sources) {
			return fmt.Errorf("%w: configuration changed during inspection", storage.ErrConflict)
		}
		for i, src := range actual {
			old := s.sources[i]
			if src.ID != old.ID || !same(src.Record, old.Record) || !same(src.Selection, old.Selection) {
				return fmt.Errorf("%w: configuration changed during inspection", storage.ErrConflict)
			}
		}
		now, e := readImage(s.common, "info/exclude")
		if e != nil {
			return e
		}
		if !same(now, before) {
			return fmt.Errorf("%w: ignore rules changed during inspection", storage.ErrConflict)
		}
		return nil
	}
	return fn(*s.w, state, warnings, recheck)
}
