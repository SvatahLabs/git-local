package hook

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// Locations of the effective hooks directory.
const (
	// LocationRepository is inside the common Git directory, like .git/hooks.
	LocationRepository = "repository"
	// LocationWorktree is inside a registered worktree, where hook scripts can
	// be committed (core.hooksPath=.githooks, husky).
	LocationWorktree = "worktree"
	// LocationShared is anywhere else, typically a global core.hooksPath that
	// other repositories use too.
	LocationShared = "shared"
)

// Location classifies a hooks directory. For LocationWorktree it also returns
// that worktree's root.
type Location struct {
	Kind, Worktree string
}

func locate(ctx context.Context, r *gitexec.Runner, w *workspace.Context, dir string) (Location, error) {
	resolved := resolveExisting(dir)
	if within(resolveExisting(w.Common), resolved) {
		return Location{Kind: LocationRepository}, nil
	}
	roots, err := workspace.RegisteredRoots(ctx, r, w)
	if err != nil {
		return Location{}, err
	}
	for _, root := range roots {
		if within(resolveExisting(root), resolved) {
			return Location{Kind: LocationWorktree, Worktree: root}, nil
		}
	}
	return Location{Kind: LocationShared}, nil
}

// checkLocation refuses directories whose hook scripts are shared with other
// repositories or can be committed, unless a shared directory is opted into.
func checkLocation(location Location, dir string, allowShared bool) error {
	const instead = "Instead, call `git local guard staged` from your pre-commit hook and `git local guard push \"$1\"` (passing the hook's standard input) from your pre-push hook."
	switch location.Kind {
	case LocationWorktree:
		return fmt.Errorf("%w: hooks directory %s is inside the worktree %s, where hook scripts can be committed and would carry this machine's binary path. %s", storage.ErrConflict, dir, location.Worktree, instead)
	case LocationShared:
		if !allowShared {
			return fmt.Errorf("%w: hooks directory %s is outside this repository and may be shared by other repositories (for example a global core.hooksPath). Rerun with --allow-shared-hooks-dir to install there. %s", storage.ErrConflict, dir, instead)
		}
	}
	return nil
}

// trackedHooks lists hook names whose files in dir are tracked by the current
// worktree's index.
func trackedHooks(ctx context.Context, r *gitexec.Runner, w *workspace.Context, dir string, hookNames []string) ([]string, error) {
	root := resolveExisting(w.Root)
	resolved := resolveExisting(dir)
	if !within(root, resolved) {
		return nil, nil
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return nil, err
	}
	tracked := []string{}
	for _, name := range hookNames {
		b, err := r.Main(ctx, w.Root, w.Admin, "ls-files", "-z", "--", filepath.ToSlash(filepath.Join(rel, name)))
		if err != nil {
			return nil, err
		}
		if len(b) > 0 {
			tracked = append(tracked, name)
		}
	}
	return tracked, nil
}

// resolveExisting resolves symlinks in the longest existing prefix of p.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, suffix)
		}
		suffix = filepath.Join(filepath.Base(p), suffix)
		p = parent
	}
}

// within compares case-insensitively on macOS and Windows, whose default
// filesystems are, so a differently cased path still counts as inside.
func within(root, p string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
