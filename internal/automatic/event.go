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
func Event(ctx context.Context, r *gitexec.Runner, cwd, event string) error {
	if event != "post-commit" && event != "post-checkout" && event != "pre-rebase" && event != "post-rewrite" {
		return errors.New("invalid automatic snapshot event")
	}
	admin, err := adminPath(ctx, r, cwd)
	if err != nil {
		return err
	}
	initialized, err := hasIdentity(admin)
	if err != nil {
		return err
	}
	if !initialized {
		if event != "post-checkout" {
			return nil
		}
		w, err := workspace.Discover(ctx, r, cwd)
		if err != nil {
			return err
		}
		roots, err := workspace.RegisteredRoots(ctx, r, w)
		if err != nil {
			return err
		}
		if len(roots) == 0 || filepath.Clean(roots[0]) == w.Root {
			return errors.New("main checkout identity is missing; cannot seed this worktree")
		}
		mainAdmin, err := r.AdminAt(ctx, roots[0])
		if err != nil {
			return err
		}
		mainInitialized, err := hasIdentity(mainAdmin)
		if err != nil {
			return err
		}
		if !mainInitialized {
			return errors.New("initialize the main checkout before creating linked worktrees")
		}
		if _, err := history.Commit(ctx, r, roots[0], "Automatic local snapshot before worktree bootstrap"); err != nil {
			return fmt.Errorf("snapshot main checkout before worktree bootstrap: %w", err)
		}
		if _, err := seed.Run(ctx, r, cwd, "main", nil); err != nil {
			return fmt.Errorf("seed new worktree from main: %w", err)
		}
		return reconcile(ctx, r, cwd)
	}
	if _, err = history.Commit(ctx, r, cwd, "Automatic local snapshot after Git "+event); err != nil {
		return err
	}
	if event == "pre-rebase" {
		return nil
	}
	return reconcile(ctx, r, cwd)
}

func reconcile(ctx context.Context, r *gitexec.Runner, cwd string) error {
	if _, err := configtx.Prune(ctx, r, cwd, false); err != nil {
		return fmt.Errorf("reconcile removed worktrees: %w", err)
	}
	return nil
}
