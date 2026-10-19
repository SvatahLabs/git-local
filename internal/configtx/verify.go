package configtx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SvatahLabs/git-local/internal/exclude"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
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
		// Any error means the worktree is unavailable, as on an offline
		// volume; its saved rules are kept without verification.
		if _, err = workspace.StatRoot(root); err != nil {
			if root == s.w.Root {
				return nil, err
			}
			continue
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
	active, err := states(s.sources, target, &next)
	if err != nil {
		return warnings, err
	}
	// Exclusions are checked only once info/exclude holds the block for
	// exactly these selections; the transitional union block still ignores
	// paths the previous selection covered.
	managed, err := s.managedLines(active)
	if err != nil {
		return warnings, err
	}
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
		// An excluded path that no workspace selects must not be ignored by
		// the managed rules, or Git hides it and git clean -X deletes it.
		excluded := []string{}
		for _, path := range state.Exclude {
			if managed == nil {
				break
			}
			// Another workspace may select the path itself, which ignores
			// everything below it too.
			if selectedByAny(active, path) {
				continue
			}
			for _, probe := range []string{path, path + "/.gitlocal-ignore-probe-" + token} {
				if !selectedByAny(active, probe) {
					excluded = append(excluded, probe)
				}
			}
		}
		matches, err := s.r.CheckIgnoreMatches(ctx, at.root, at.admin, excluded)
		if err != nil {
			return warnings, err
		}
		for _, path := range excluded {
			match := matches[path]
			if match.Ignored && filepath.Base(match.Source) == "exclude" && filepath.Base(filepath.Dir(match.Source)) == "info" && managed[match.Pattern] {
				remedy := ""
				if outdated, err := s.ignoreOutdated(); err == nil && outdated {
					remedy = "; an earlier release wrote this block: run git local init to rewrite it"
				}
				return warnings, &validationError{fmt.Errorf("%w: excluded path %q in workspace %s is still ignored by managed rule %q%s", storage.ErrConflict, filepath.ToSlash(path), src.ID, match.Pattern, remedy)}
			}
		}
	}
	return warnings, nil
}

func selectedByAny(active []selection.State, path string) bool {
	for _, state := range active {
		if state.Selected(path) {
			return true
		}
	}
	return false
}

// managedLines returns the pattern lines inside the managed block of
// info/exclude, or nil when that block isn't the one for active.
func (s *session) managedLines(active []selection.State) (map[string]bool, error) {
	current, err := readImage(s.common, "info/exclude")
	if err != nil {
		return nil, err
	}
	want, alternates, err := s.compile(active)
	if err != nil {
		return nil, err
	}
	if _, err := exclude.UserBytes(current.Data, want, alternates...); err != nil {
		return nil, nil
	}
	lines := map[string]bool{}
	inside := false
	for _, line := range strings.Split(string(current.Data), "\n") {
		switch {
		case line+"\n" == exclude.Begin:
			inside = true
		case line+"\n" == exclude.End:
			inside = false
		case inside:
			lines[line] = true
		}
	}
	return lines, nil
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
