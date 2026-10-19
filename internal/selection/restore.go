package selection

import (
	"fmt"
	"github.com/SvatahLabs/git-local/internal/storage"
	"os"
	"strings"
)

// CheckRestorePath checks only the requested path and its ancestor boundaries.
// It does not traverse or capture unrelated current-only directory siblings.
func (e *Engine) CheckRestorePath(path string) error { return e.CheckRestorePaths([]string{path}) }

// CheckRestorePaths is CheckRestorePath for each path. It reads each
// directory once, so checking every file of a large directory doesn't cost a
// read of that directory per file.
func (e *Engine) CheckRestorePaths(paths []string) error {
	c := restoreCheck{e: e, infos: map[string]os.FileInfo{}, entries: map[string][]os.DirEntry{}, aliases: map[string]map[string][]string{}, nested: map[string]error{}}
	for _, path := range paths {
		if err := c.check(path); err != nil {
			return err
		}
	}
	return nil
}

// restoreCheck caches what CheckRestorePaths reads.
type restoreCheck struct {
	e       *Engine
	infos   map[string]os.FileInfo // nil when absent
	entries map[string][]os.DirEntry
	aliases map[string]map[string][]string
	nested  map[string]error
}

func (c *restoreCheck) lstat(name string) (os.FileInfo, error) {
	if info, ok := c.infos[name]; ok {
		return info, nil
	}
	info, err := c.e.files.Lstat(name)
	if os.IsNotExist(err) {
		info, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.infos[name] = info
	return info, nil
}

// readDir returns dir's entries and its names by alias key; it reports
// false when dir doesn't exist.
func (c *restoreCheck) readDir(dir string) ([]os.DirEntry, map[string][]string, bool, error) {
	if entries, ok := c.entries[dir]; ok {
		return entries, c.aliases[dir], entries != nil, nil
	}
	entries, err := c.e.files.ReadDir(dir)
	if os.IsNotExist(err) {
		c.entries[dir] = nil
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if entries == nil {
		entries = []os.DirEntry{}
	}
	names := map[string][]string{}
	for _, entry := range entries {
		key := storage.AliasKey(entry.Name())
		names[key] = append(names[key], entry.Name())
	}
	c.entries[dir], c.aliases[dir] = entries, names
	return entries, names, true, nil
}

func (c *restoreCheck) check(path string) error {
	if err := storage.ValidateSelectionPath(path); err != nil {
		return err
	}
	parts := strings.Split(path, "/")
	// Ancestor boundaries, as Engine.boundaries.
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		fi, err := c.lstat(parent)
		if err != nil {
			return err
		}
		if fi == nil {
			break
		}
		if !fi.IsDir() {
			return fmt.Errorf("unsafe non-directory ancestor %q", parent)
		}
		if err = storage.ValidateInfo(fi); err != nil {
			return err
		}
		nested, ok := c.nested[parent]
		if !ok {
			entries, _, _, err := c.readDir(parent)
			if err != nil {
				return err
			}
			nested = nestedRepository(parent, entries)
			c.nested[parent] = nested
		}
		if nested != nil {
			return nested
		}
	}
	for i, part := range parts {
		parent := strings.Join(parts[:i], "/")
		_, names, exists, err := c.readDir(parent)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		for _, name := range names[storage.AliasKey(part)] {
			if name != part {
				return fmt.Errorf("%w: path alias obstructs %q", storage.ErrConflict, path)
			}
		}
		if i < len(parts)-1 {
			fi, err := c.lstat(strings.Join(parts[:i+1], "/"))
			if err != nil {
				return err
			}
			if fi == nil {
				return nil
			}
			if !fi.IsDir() {
				return fmt.Errorf("%w: unsafe ancestor of %q", storage.ErrConflict, path)
			}
		}
	}
	return nil
}

// CheckRestoreAncestors is CheckRestorePath without the directory scans: each
// existing ancestor must be a plain directory. It costs a stat per ancestor,
// for re-checks between full checks.
func (e *Engine) CheckRestoreAncestors(path string) error {
	if err := storage.ValidateSelectionPath(path); err != nil {
		return err
	}
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		fi, err := e.files.Lstat(parent)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: unsafe ancestor of %q", storage.ErrConflict, path)
		}
		if err = storage.ValidateInfo(fi); err != nil {
			return err
		}
	}
	return nil
}
