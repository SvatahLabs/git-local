package hook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
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

// locate picks the most specific (deepest) root containing dir, so a worktree
// nested inside the common Git directory still counts as a worktree, and the
// common directory nested inside the main worktree counts as the repository.
func locate(ctx context.Context, r *gitexec.Runner, w *workspace.Context, dir string) (Location, error) {
	resolved := resolveExisting(dir)
	roots, err := workspace.RegisteredRoots(ctx, r, w)
	if err != nil {
		return Location{}, err
	}
	best, bestLen := Location{Kind: LocationShared}, -1
	if common := resolveExisting(w.Common); within(common, resolved) {
		best, bestLen = Location{Kind: LocationRepository}, len(common)
	}
	for _, root := range roots {
		if resolvedRoot := resolveExisting(root); within(resolvedRoot, resolved) && len(resolvedRoot) > bestLen {
			best, bestLen = Location{Kind: LocationWorktree, Worktree: root}, len(resolvedRoot)
		}
	}
	return best, nil
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

// within reports whether p is root or inside it. It compares root with p's
// existing ancestors by file identity, so it follows the filesystem's own
// rules for case and Unicode normalization: a differently cased or normalized
// spelling counts on a default macOS or Windows volume, but not on a
// case-sensitive one. A root that doesn't exist is compared by path.
func within(root, p string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		rel, err := filepath.Rel(root, p)
		return err == nil && (rel == "." || filepath.IsLocal(rel))
	}
	for dir := filepath.Clean(p); ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil && os.SameFile(rootInfo, info) {
			return true
		}
		if filepath.Dir(dir) == dir {
			return false
		}
	}
}
