package configtx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type location struct{ root, admin string }
type validationError struct{ err error }

func (e *validationError) Error() string { return e.err.Error() }
func (e *validationError) Unwrap() error { return e.err }

func (s *session) locations(ctx context.Context) (map[string]location, error) {
	roots, err := workspace.RegisteredRoots(ctx, s.r, s.w)
	if err != nil {
		return nil, err
	}
	found := map[string]location{}
	for _, root := range roots {
		if _, err = os.Stat(root); err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				continue
			}
			return nil, err
		}
		admin := s.w.Admin
		if root != s.w.Root {
			admin, err = s.r.AdminAt(ctx, root)
			if err != nil {
				continue
			}
		}
		d, err := storage.OpenDir(admin)
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				continue
			}
			return nil, err
		}
		var id workspace.Identity
		err = d.ReadJSON("gitlocal-workspace.json", &id)
		d.Close()
		if os.IsNotExist(err) || os.IsPermission(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if id.SchemaVersion != workspace.SchemaVersion || id.RepositoryID != s.w.RepositoryID || !storage.ValidUUID(id.WorkspaceID) {
			return nil, fmt.Errorf("%w: inconsistent registered workspace identity", storage.ErrConflict)
		}
		if old, ok := found[id.WorkspaceID]; ok && old.admin != admin {
			return nil, fmt.Errorf("%w: duplicate registered workspace identity", storage.ErrConflict)
		}
		found[id.WorkspaceID] = location{root, admin}
	}
	return found, nil
}

// verify checks actual selected files and representative future names in every
// selected directory. Future arbitrary ignore-rule changes are rechecked on the
// next operation; these probes are not a promise about every future filename.
func (s *session) verify(ctx context.Context, target string, next selection.State, soft bool) ([]string, error) {
	available, err := s.locations(ctx)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	for _, src := range s.sources {
		record, err := src.record()
		if err != nil {
			return warnings, err
		}
		if record.State != "active" {
			continue
		}
		state, err := src.state()
		if err != nil {
			return warnings, err
		}
		if src.ID == target {
			state = next
		}
		at, ok := available[src.ID]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("Workspace %s at %q is unavailable; its saved rules remain in the shared union and could not be verified.", src.ID, record.Root))
			continue
		}
		engine, err := selection.New(s.r, at.root, at.admin)
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				warnings = append(warnings, fmt.Sprintf("Workspace %s is unavailable; saved rules retained without verification.", src.ID))
				continue
			}
			return warnings, err
		}
		plan, err := engine.Validate(ctx, state)
		engine.Close()
		if err != nil {
			if soft {
				warnings = append(warnings, fmt.Sprintf("Workspace %s has remaining selection conflicts: %v", src.ID, err))
				continue
			}
			return warnings, &validationError{err}
		}
		paths := map[string]bool{}
		for _, entry := range plan.Entries {
			paths[entry.Path] = true
		}
		dirs := append([]string{}, plan.Directories...)
		for _, rule := range plan.Missing {
			if rule.Kind == "directory" {
				dirs = append(dirs, rule.Path)
			} else {
				paths[rule.Path] = true
			}
		}
		token, err := storage.UUID()
		if err != nil {
			return warnings, err
		}
		for _, dir := range dirs {
			probe := dir + "/.gitlocal-ignore-probe-" + token
			if state.Selected(probe) {
				paths[probe] = true
			}
		}
		batch := []string{}
		for path := range paths {
			batch = append(batch, path)
		}
		sort.Strings(batch)
		ignored, err := s.r.CheckIgnore(ctx, at.root, at.admin, batch)
		if err != nil {
			return warnings, err
		}
		for _, path := range batch {
			if !ignored[path] {
				problem := fmt.Errorf("%w: selected path %q in workspace %s is not effectively ignored; reconcile overriding user ignore rules", storage.ErrConflict, filepath.ToSlash(path), src.ID)
				if soft {
					warnings = append(warnings, problem.Error())
				} else {
					return warnings, &validationError{problem}
				}
			}
		}
	}
	return warnings, nil
}
func uniqueWarnings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, w := range in {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}
func isValidation(err error) bool { var e *validationError; return errors.As(err, &e) }
