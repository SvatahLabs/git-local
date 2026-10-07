package automatic

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type RemoveResult struct {
	WorkspaceID     string
	SnapshotChanged bool
	Removed         bool
	Retired         bool
	TransactionID   string
	Warnings        []string
}

// RemoveWorktree saves selected files, asks Git to remove one available linked
// worktree, then retires its record. If retirement stops after Git removal,
// the journaled workspace prune/recover commands can finish it safely.
func RemoveWorktree(ctx context.Context, r *gitexec.Runner, cwd, target string, force bool) (RemoveResult, error) {
	var result RemoveResult
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(cwd, target)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return result, err
	}
	entries, err := configtx.ListWorkspaces(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	var found bool
	for _, entry := range entries {
		if filepath.Clean(entry.Root) != target {
			continue
		}
		if found {
			return result, fmt.Errorf("%w: ambiguous linked worktree path", storage.ErrConflict)
		}
		if !entry.Registered || !entry.Available || entry.Locked || entry.State != "active" && entry.State != "moved" || entry.Admin == w.Common || entry.ID == w.WorkspaceID {
			return result, fmt.Errorf("%w: target must be an available, unlocked, distinct linked worktree", storage.ErrConflict)
		}
		result.WorkspaceID = entry.ID
		found = true
	}
	if !found {
		return result, errors.New("target is not an initialized linked worktree")
	}
	saved, err := history.Commit(ctx, r, target, "Automatic local snapshot before worktree removal")
	result.Warnings = append(result.Warnings, saved.Warnings...)
	if err != nil {
		return result, fmt.Errorf("snapshot linked worktree before removal: %w", err)
	}
	result.SnapshotChanged = saved.Changed
	// Removal deletes ignored files too, so a skipped file whose content
	// differs from its carried version would be lost. Unchanged or deleted
	// skipped files lose nothing.
	if len(saved.Unsaved) > 0 && !force {
		return result, fmt.Errorf("%w: the snapshot before removal skipped %s, whose changes would be lost; correct the sensitive-file policy (see git local doctor), or use --force to discard them", storage.ErrConflict, quoted(saved.Unsaved))
	}
	if err := r.RemoveLinkedWorktree(ctx, w.Root, w.Admin, target, force); err != nil {
		return result, fmt.Errorf("%w: Git refused to remove %q after the local snapshot; use --force only if other worktree changes may be discarded: %v", storage.ErrConflict, target, err)
	}
	result.Removed = true
	retired, err := configtx.Retire(ctx, r, cwd, result.WorkspaceID)
	result.TransactionID = retired.TransactionID
	result.Warnings = append(result.Warnings, retired.Warnings...)
	if err != nil {
		return result, fmt.Errorf("%w: Git removed worktree %s, but retirement needs recovery: %v; run git local workspace prune or git local recover", storage.ErrConflict, result.WorkspaceID, err)
	}
	result.Retired = true
	return result, nil
}

func quoted(paths []string) string {
	list := make([]string, len(paths))
	for i, path := range paths {
		list[i] = strconv.Quote(path)
	}
	return strings.Join(list, ", ")
}
