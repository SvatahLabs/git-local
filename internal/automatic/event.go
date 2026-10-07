// Package automatic handles Git events without turning a post-event failure
// into an undo of an already completed mainline operation.
package automatic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/seed"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func adminPath(ctx context.Context, r *gitexec.Runner, cwd string) (string, error) {
	b, err := r.Guard(ctx, cwd, "", "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	admin := strings.TrimSuffix(string(b), "\n")
	if !filepath.IsAbs(admin) || strings.ContainsAny(admin, "\x00\r\n") {
		return "", errors.New("invalid Git administrative path")
	}
	return filepath.Clean(admin), nil
}

func hasIdentity(admin string) (bool, error) {
	d, err := storage.OpenDir(admin)
	if err != nil {
		return false, err
	}
	defer d.Close()
	info, err := d.Lstat("gitlocal-workspace.json")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("invalid worktree identity file")
	}
	return true, nil
}

// Event captures changed local files. A post-checkout in a newly created
// linked worktree first snapshots the main checkout, then seeds the new one.
// It returns the warnings of the snapshots, seed and reconciliation it ran.
func Event(ctx context.Context, r *gitexec.Runner, cwd, event string) ([]string, error) {
	if event != "post-commit" && event != "post-checkout" && event != "pre-rebase" && event != "post-rewrite" {
		return nil, errors.New("invalid automatic snapshot event")
	}
	admin, err := adminPath(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	initialized, err := hasIdentity(admin)
	if err != nil {
		return nil, err
	}
	if !initialized {
		if event != "post-checkout" {
			return nil, nil
		}
		w, err := workspace.Discover(ctx, r, cwd)
		if err != nil {
			return nil, err
		}
		if bare, err := workspace.Bare(ctx, r, w); err != nil {
			return nil, err
		} else if bare {
			return []string{"this is a worktree of a bare repository, which has no main checkout to seed it from; run git local init here, or git local init --from <workspace UUID>"}, nil
		}
		roots, err := workspace.RegisteredRoots(ctx, r, w)
		if err != nil {
			return nil, err
		}
		if len(roots) == 0 || filepath.Clean(roots[0]) == w.Root {
			return nil, errors.New("main checkout identity is missing; cannot seed this worktree")
		}
		mainAdmin, err := r.AdminAt(ctx, roots[0])
		if err != nil {
			return nil, err
		}
		mainInitialized, err := hasIdentity(mainAdmin)
		if err != nil {
			return nil, err
		}
		if !mainInitialized {
			return nil, errors.New("initialize the main checkout before creating linked worktrees")
		}
		saved, err := history.Commit(ctx, r, roots[0], "Automatic local snapshot before worktree bootstrap")
		warnings := saved.Warnings
		if err != nil {
			return warnings, fmt.Errorf("snapshot main checkout before worktree bootstrap: %w", err)
		}
		seeded, err := seed.Run(ctx, r, cwd, "main", nil)
		warnings = append(warnings, seeded.Warnings...)
		if err != nil {
			return warnings, fmt.Errorf("seed new worktree from main: %w", err)
		}
		return reconcile(ctx, r, cwd, warnings)
	}
	saved, err := history.Commit(ctx, r, cwd, "Automatic local snapshot after Git "+event)
	if err != nil || event == "pre-rebase" {
		return saved.Warnings, err
	}
	return reconcile(ctx, r, cwd, saved.Warnings)
}

func reconcile(ctx context.Context, r *gitexec.Runner, cwd string, warnings []string) ([]string, error) {
	pruned, err := configtx.Prune(ctx, r, cwd, false)
	warnings = append(warnings, pruned.Warnings...)
	if err != nil {
		return warnings, fmt.Errorf("reconcile removed worktrees: %w", err)
	}
	return warnings, nil
}
