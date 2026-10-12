// Package teardown removes git-local from a repository (git local deinit).
package teardown

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/hook"
	"gitlocal/internal/overlay"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// Result reports what deinit removed. Archive is the kept private state,
// relative to the common Git directory; it's empty when nothing was kept.
type Result struct {
	Overlays   []string `json:"overlays"`
	Hooks      []string `json:"hooks"`
	Exclude    bool     `json:"excludeBlockRemoved"`
	Identities []string `json:"identities"`
	Archive    string   `json:"archive,omitempty"`
	Removed    bool     `json:"historyRemoved"`
	Warnings   []string `json:"warnings"`
}

// Deinit removes git-local from the repository at cwd and every worktree:
// overlays, hooks, the managed info/exclude block, the private state and each
// worktree's identity file. It refuses while a transaction is pending or an
// overlay's working file holds changes that aren't saved in the overlay.
//
// Each step can be run again, so an interrupted deinit is finished by running
// it again. The private state directory is renamed (kept) in one step, after
// everything that needs it; removeHistory deletes it instead, only when no
// quarantine or restore recovery ref would be lost.
func Deinit(ctx context.Context, r *gitexec.Runner, cwd string, removeHistory bool) (Result, error) {
	result := Result{Overlays: []string{}, Hooks: []string{}, Identities: []string{}, Warnings: []string{}}
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return result, err
	}
	defer common.Close()
	if _, err := common.Lstat("gitlocal"); err == nil {
		if err := checkRemovable(ctx, r, w, removeHistory); err != nil {
			return result, err
		}
		if result.Overlays, err = removeOverlays(ctx, r, w); err != nil {
			return result, err
		}
		for _, uninstall := range []func() ([]string, []string, error){
			func() ([]string, []string, error) { return hook.UninstallSnapshots(ctx, r, cwd) },
			func() ([]string, []string, error) { return hook.Uninstall(ctx, r, cwd, false, false) },
		} {
			paths, warnings, err := uninstall()
			result.Hooks = append(result.Hooks, paths...)
			result.Warnings = append(result.Warnings, warnings...)
			if err != nil {
				return result, err
			}
		}
		if result.Exclude, err = configtx.RemoveExcludeBlock(ctx, r, cwd); err != nil {
			return result, err
		}
		id, err := storage.UUID()
		if err != nil {
			return result, err
		}
		archive := "gitlocal-removed-" + id
		if err := common.RenameNoReplace("gitlocal", archive); err != nil {
			return result, err
		}
		if removeHistory {
			if err := os.RemoveAll(filepath.Join(w.Common, archive)); err != nil {
				return result, fmt.Errorf("delete the private state %s: %w", filepath.Join(w.Common, archive), err)
			}
			result.Removed = true
		} else {
			result.Archive = archive
		}
	} else if !os.IsNotExist(err) {
		return result, err
	}
	result.Identities, err = removeIdentities(common, w.Common)
	return result, err
}

// checkRemovable refuses what deinit can't undo safely: a pending
// transaction, and with removeHistory, a quarantine or recovery ref.
func checkRemovable(ctx context.Context, r *gitexec.Runner, w *workspace.Context, removeHistory bool) error {
	pending, err := configtx.PendingJournals(ctx, r, w.Root)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: transaction %s is unfinished; resolve it first (git local doctor names the command)", storage.ErrConflict, pending[0].ID)
	}
	if !removeHistory {
		return nil
	}
	var kept []string
	quarantine, err := os.ReadDir(filepath.Join(w.Common, "gitlocal", "quarantine"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range quarantine {
		kept = append(kept, filepath.Join(w.Common, "gitlocal", "quarantine", entry.Name()))
	}
	refs, err := r.HistoryListing(ctx, w.Store, nil, "show-ref")
	var ge *gitexec.Error
	if err != nil && !(errors.As(err, &ge) && ge.Code == 1) {
		return err
	}
	for _, line := range strings.Split(string(bytes.TrimSpace(refs)), "\n") {
		if _, ref, ok := strings.Cut(line, " "); ok && strings.HasPrefix(ref, "refs/gitlocal/recovery/") {
			kept = append(kept, ref)
		}
	}
	if len(kept) > 0 {
		return fmt.Errorf("%w: --remove-history would delete %s; check and delete them, or run git local deinit without --remove-history to keep the history", storage.ErrConflict, strings.Join(kept, ", "))
	}
	return nil
}

// removeOverlays deactivates every overlay. Each must be removed from its own
// worktree, or from here when its workspace is retained.
func removeOverlays(ctx context.Context, r *gitexec.Runner, w *workspace.Context) ([]string, error) {
	versions, err := overlay.Registered(ctx, w.Common)
	if err != nil || len(versions) == 0 {
		return []string{}, err
	}
	workspaces, err := configtx.ListWorkspaces(ctx, r, w.Root)
	if err != nil {
		return nil, err
	}
	byID := map[string]configtx.WorkspaceInfo{}
	for _, info := range workspaces {
		byID[info.ID] = info
	}
	var problems []string
	for _, v := range versions {
		info := byID[v.Manifest.WorkspaceID]
		switch {
		case info.State == "retained":
		case info.Available:
			if v.Manifest.State == "removing" {
				continue
			}
			working, err := os.ReadFile(filepath.Join(info.Root, filepath.FromSlash(v.Manifest.Path)))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if err == nil && !bytes.Equal(working, v.Local) {
				problems = append(problems, fmt.Sprintf("%s in %s has changes the overlay %s doesn't hold (save them with git local overlay add, or put back the saved version: git local overlay show %s)", v.Manifest.Path, info.Root, v.Manifest.Name, v.Manifest.Name))
			}
		default:
			problems = append(problems, fmt.Sprintf("overlay %s belongs to workspace %s, whose worktree is %s; reconnect it, or prune it with git local workspace prune", v.Manifest.Name, v.Manifest.WorkspaceID, info.State))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", storage.ErrConflict, strings.Join(problems, "; "))
	}
	removed := []string{}
	for _, v := range versions {
		info := byID[v.Manifest.WorkspaceID]
		var err error
		if info.State == "retained" {
			_, err = overlay.RemoveFor(ctx, r, w.Root, v.Manifest.WorkspaceID, v.Manifest.Name)
		} else {
			_, err = overlay.Remove(ctx, r, info.Root, v.Manifest.Name)
		}
		if err != nil {
			return removed, fmt.Errorf("remove overlay %s: %w", v.Manifest.Name, err)
		}
		removed = append(removed, v.Manifest.Name)
	}
	return removed, nil
}

// removeIdentities deletes the workspace identity file of the main worktree
// (in the common directory) and of each linked worktree.
func removeIdentities(common *storage.Dir, commonPath string) ([]string, error) {
	names := []string{"gitlocal-workspace.json"}
	linked, err := os.ReadDir(filepath.Join(commonPath, "worktrees"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range linked {
		if entry.IsDir() {
			names = append(names, filepath.Join("worktrees", entry.Name(), "gitlocal-workspace.json"))
		}
	}
	removed := []string{}
	for _, name := range names {
		if err := common.Remove(name); err == nil {
			removed = append(removed, filepath.Join(commonPath, name))
		} else if !os.IsNotExist(err) {
			return removed, err
		}
	}
	return removed, nil
}
