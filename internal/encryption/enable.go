package encryption

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// EnableFresh publishes a sensitive-file policy only when the private history
// store has no refs or objects. Existing history needs an atomic rewrite before
// policy publication and is deliberately rejected here.
func EnableFresh(ctx context.Context, r *gitexec.Runner, cwd, keyFile string, paths []string) (secureblob.Policy, error) {
	var published secureblob.Policy
	if len(paths) == 0 {
		return published, errors.New("at least one sensitive path is required")
	}
	_, err := configtx.WithSelection(ctx, r, cwd, func(w workspace.Context, state selection.State, recheck func() error) error {
		engine, err := selection.New(r, w.Root, w.Admin)
		if err != nil {
			return err
		}
		defer engine.Close()
		normalized := make([]string, 0, len(paths))
		for _, arg := range paths {
			path, e := engine.Normalize(cwd, arg)
			if e != nil {
				return e
			}
			if !state.Selected(path) {
				return fmt.Errorf("%w: %q is not selected for local history", storage.ErrConflict, path)
			}
			normalized = append(normalized, path)
		}
		rules, err := sensitiveRules(w.Root, state, normalized, nil)
		if err != nil {
			return err
		}
		policy, err := secureblob.PolicyForKey(keyFile, w.Root, w.Common, rules)
		if err != nil {
			return err
		}
		common, err := storage.OpenDir(w.Common)
		if err != nil {
			return err
		}
		defer common.Close()
		if _, exists, e := secureblob.ReadLocked(common, w.Root, w.Common); e != nil {
			return e
		} else if exists {
			return fmt.Errorf("%w: sensitive-file policy already exists", storage.ErrConflict)
		}
		roots, err := workspace.RegisteredRoots(ctx, r, &w)
		if err != nil {
			return err
		}
		for _, root := range roots {
			if _, err := os.Stat(root); err != nil {
				return fmt.Errorf("%w: verify or prune unavailable worktree %q before enabling encryption: %v", storage.ErrConflict, root, err)
			}
			if _, err := secureblob.Canonical(policy, root, w.Common); err != nil {
				return err
			}
		}
		if err := emptyHistoryStore(w.Store); err != nil {
			return err
		}
		if err := recheck(); err != nil {
			return err
		}
		// There are no old objects to migrate. Atomic policy publication is
		// the only state transition; interruption cannot expose old plaintext.
		if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
			return err
		}
		published = policy
		return nil
	})
	return published, err
}

func emptyHistoryStore(store string) error {
	entries, err := os.ReadDir(filepath.Join(store, "objects"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "info" && entry.Name() != "pack" {
			return fmt.Errorf("%w: private history already has objects; encryption migration is required", storage.ErrConflict)
		}
		if !entry.IsDir() {
			return fmt.Errorf("%w: unsafe private object directory", storage.ErrConflict)
		}
		children, err := os.ReadDir(filepath.Join(store, "objects", entry.Name()))
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return fmt.Errorf("%w: private history already has objects; encryption migration is required", storage.ErrConflict)
		}
	}
	for _, name := range []string{"refs", "packed-refs", "logs"} {
		path := filepath.Join(store, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if name != "refs" || !info.IsDir() {
			return fmt.Errorf("%w: private history already has refs; encryption migration is required", storage.ErrConflict)
		}
		err = filepath.WalkDir(path, func(child string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if child != path && !entry.IsDir() {
				return fmt.Errorf("%w: private history already has refs; encryption migration is required", storage.ErrConflict)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Keygen creates an external owner-private key for a checkout.
func Keygen(ctx context.Context, r *gitexec.Runner, cwd, keyFile string) error {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(keyFile) || strings.TrimSpace(keyFile) == "" {
		return errors.New("encryption key path must be absolute")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(keyFile))
	if err != nil {
		return err
	}
	resolvedKey := filepath.Join(parent, filepath.Base(keyFile))
	roots, err := workspace.RegisteredRoots(ctx, r, w)
	if err != nil {
		return err
	}
	for _, root := range append(roots, w.Common) {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedKey)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return errors.New("sensitive-file key must be outside every checkout and the common Git directory")
		}
	}
	return keymaterial.Create(keyFile, w.Root)
}

// Rebind changes only the external key path. The replacement key must
// authenticate against the existing fingerprint; ciphertext is not rewritten.
func Rebind(ctx context.Context, r *gitexec.Runner, cwd, keyFile string) (secureblob.Policy, error) {
	var updated secureblob.Policy
	_, err := configtx.WithSelection(ctx, r, cwd, func(w workspace.Context, _ selection.State, recheck func() error) error {
		common, err := storage.OpenDir(w.Common)
		if err != nil {
			return err
		}
		defer common.Close()
		policy, exists, err := secureblob.ReadLocked(common, w.Root, w.Common)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: sensitive-file encryption is not enabled", storage.ErrConflict)
		}
		policy, err = secureblob.RebindKeyPath(policy, keyFile, w.Root, w.Common)
		if err != nil {
			return err
		}
		roots, err := workspace.RegisteredRoots(ctx, r, &w)
		if err != nil {
			return err
		}
		for _, root := range roots {
			if _, err := os.Stat(root); err != nil {
				return fmt.Errorf("%w: verify or prune unavailable worktree %q before rebinding encryption key: %v", storage.ErrConflict, root, err)
			}
			if _, err := secureblob.Canonical(policy, root, w.Common); err != nil {
				return err
			}
		}
		if err := recheck(); err != nil {
			return err
		}
		if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
			return err
		}
		updated = policy
		return nil
	})
	return updated, err
}
