package selection

import (
	"fmt"
	"gitlocal/internal/storage"
	"os"
	"strings"
)

// CheckRestorePath checks only the requested path and its ancestor boundaries.
// It does not traverse or capture unrelated current-only directory siblings.
func (e *Engine) CheckRestorePath(path string) error {
	if err := storage.ValidateSelectionPath(path); err != nil {
		return err
	}
	if err := e.boundaries(path); err != nil {
		return err
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parent := strings.Join(parts[:i], "/")
		entries, err := e.files.ReadDir(parent)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if storage.AliasKey(entry.Name()) == storage.AliasKey(part) && entry.Name() != part {
				return fmt.Errorf("%w: path alias obstructs %q", storage.ErrConflict, path)
			}
		}
		if i < len(parts)-1 {
			fi, err := e.files.Lstat(strings.Join(parts[:i+1], "/"))
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				return fmt.Errorf("%w: unsafe ancestor of %q", storage.ErrConflict, path)
			}
		}
	}
	return nil
}
