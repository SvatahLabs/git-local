package automatic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/overlay"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type ResetResult struct {
	SnapshotID      string
	SnapshotChanged bool
	Commit          string
	Reset           bool
	Warnings        []string
}

// ResetHard is an explicit destructive-reset boundary. Git has no pre-reset
// hook, so callers use this command when they want a guaranteed local capture.
func ResetHard(ctx context.Context, r *gitexec.Runner, cwd, revision string) (ResetResult, error) {
	var result ResetResult
	if revision == "" || strings.HasPrefix(revision, "-") || strings.ContainsAny(revision, "\x00\r\n") {
		return result, errors.New("invalid reset revision")
	}
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	resolved, err := r.Main(ctx, w.Root, w.Admin, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return result, fmt.Errorf("resolve reset revision %q: %w", revision, err)
	}
	result.Commit = strings.TrimSuffix(string(resolved), "\n")
	paths, err := checkResetTarget(ctx, r, w, result.Commit)
	if err != nil {
		return result, err
	}
	if err := checkResetFilters(ctx, r, w, result.Commit, paths); err != nil {
		return result, err
	}
	saved, err := history.Commit(ctx, r, w.Root, "Automatic local snapshot before Git reset --hard")
	if err != nil {
		return result, fmt.Errorf("snapshot selected files before reset: %w", err)
	}
	result.SnapshotID = saved.ID
	result.SnapshotChanged = saved.Changed
	result.Warnings = saved.Warnings
	if err := r.ResetHard(ctx, w.Root, w.Admin, result.Commit); err != nil {
		snapshot := "pre-reset capture"
		if result.SnapshotID != "" {
			snapshot = "local snapshot " + result.SnapshotID
		}
		return result, fmt.Errorf("%w: Git reset failed after %s; inspect the main worktree before retrying: %v", storage.ErrConflict, snapshot, err)
	}
	result.Reset = true
	return result, nil
}

func checkResetTarget(ctx context.Context, r *gitexec.Runner, w *workspace.Context, commit string) ([]string, error) {
	snapshot, err := configtx.LoadGuardSnapshot(ctx, r, w.Root)
	if err != nil {
		return nil, err
	}
	data, err := r.Main(ctx, w.Root, w.Admin, "ls-tree", "--full-tree", "--name-only", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	if len(data) != 0 && data[len(data)-1] != 0 {
		return nil, errors.New("malformed reset target tree")
	}
	paths := make([]string, 0)
	for _, path := range bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
		if len(path) != 0 {
			paths = append(paths, string(path))
		}
	}
	if len(snapshot.Selections) != 0 {
		conflicts, err := snapshot.Check(paths)
		if err != nil {
			return nil, err
		}
		if len(conflicts) != 0 {
			return nil, fmt.Errorf("%w: reset target contains selected path %q from workspace %s", storage.ErrConflict, conflicts[0].Path, conflicts[0].WorkspaceID)
		}
		for _, selected := range snapshot.Selections {
			if err := (selection.Ownership{Paths: paths}).Check(selected.State); err != nil {
				return nil, fmt.Errorf("%w: reset target blocks selection from workspace %s: %v", storage.ErrConflict, selected.WorkspaceID, err)
			}
		}
	}
	return paths, nil
}

// A hard reset may invoke checkout filters. Permit only git-local's audited
// overlay process, so a configured third-party process cannot run as part of
// an extension command.
func checkResetFilters(ctx context.Context, r *gitexec.Runner, w *workspace.Context, commit string, paths []string) error {
	if err := overlay.AuditInstallation(ctx, r, w.Root, w.Common); err != nil {
		return fmt.Errorf("%w: overlay installation is not safe for reset: %v", storage.ErrConflict, err)
	}
	attributes, err := r.ResetFilters(ctx, w.Root, w.Admin, commit, paths)
	if err != nil {
		return err
	}
	active := make(map[string]bool)
	for _, name := range attributes {
		active[strings.ToLower(name)] = true
	}
	config, err := r.Main(ctx, w.Root, w.Admin, "config", "--null", "--list")
	if err != nil {
		return err
	}
	var overlayProcess bool
	for _, record := range bytes.Split(config, []byte{0}) {
		key, _, ok := strings.Cut(string(record), "\n")
		if !ok || !strings.HasPrefix(key, "filter.") {
			continue
		}
		if !strings.HasSuffix(key, ".process") && !strings.HasSuffix(key, ".smudge") {
			continue
		}
		name := strings.TrimPrefix(key, "filter.")
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".process"), ".smudge")
		if !active[strings.ToLower(name)] {
			continue
		}
		if strings.EqualFold(name, "gitlocal-overlay") && strings.HasSuffix(key, ".process") {
			overlayProcess = true
			continue
		}
		return fmt.Errorf("%w: hard reset would run configured checkout filter %q; use Git directly after saving a local snapshot", storage.ErrConflict, key)
	}
	if overlayProcess {
		if err := overlay.AuditResetFilter(ctx, r, w.Root, w.Admin, w.Common); err != nil {
			return fmt.Errorf("%w: overlay filter is not safe for reset: %v", storage.ErrConflict, err)
		}
	}
	return nil
}
