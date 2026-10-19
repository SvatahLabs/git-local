package backup

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/keymaterial"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// CreateKeyFile writes a new raw key with owner-only permissions. It refuses
// existing files and paths within the checkout.
func CreateKeyFile(path, repoRoot string) error {
	return keymaterial.Create(path, repoRoot)
}

// LoadKeyFile accepts only an owner-private raw key outside the checkout.
func LoadKeyFile(path, repoRoot string) ([]byte, error) {
	return keymaterial.Load(path, repoRoot)
}

// Keygen creates a backup key outside every registered worktree and the
// common Git directory, as encrypt keygen does.
func Keygen(ctx context.Context, r *gitexec.Runner, cwd, path string) error {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return err
	}
	if err := CheckKeyLocation(ctx, r, *w, path); err != nil {
		return err
	}
	return CreateKeyFile(path, w.Root)
}

// CheckKeyLocation refuses a backup key inside any registered worktree or the
// common Git directory. From a linked worktree, the current root alone would
// let the key land in the main checkout or under .git.
func CheckKeyLocation(ctx context.Context, r *gitexec.Runner, w workspace.Context, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("backup key path must be absolute")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	resolved := filepath.Join(parent, filepath.Base(path))
	roots, err := workspace.RegisteredRoots(ctx, r, &w)
	if err != nil {
		return err
	}
	for _, root := range append(roots, w.Common) {
		if inside, err := keymaterial.Inside(root, resolved); err != nil {
			return err
		} else if inside {
			return errors.New("backup key must be outside every checkout and the common Git directory")
		}
	}
	return nil
}
