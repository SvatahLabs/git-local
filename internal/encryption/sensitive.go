package encryption

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SvatahLabs/git-local/internal/secureblob"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
)

// sensitiveRules takes each rule's kind from an exact selection include rule,
// or else from the checkout: a directory below a tracked directory must cover
// its descendants. A path that is missing or spelled differently on disk is
// refused, because a file rule for it would silently protect nothing.
//
// history lists the paths recorded by existing snapshots (nil for a fresh
// store). A path with recorded descendants is a directory rule whatever the
// checkout holds now, and a path that history records may be missing on disk.
func sensitiveRules(root string, state selection.State, paths []string, history map[string]bool) ([]secureblob.PathRule, error) {
	files, err := storage.OpenDir(root)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	rules := make([]secureblob.PathRule, 0, len(paths))
	for _, path := range paths {
		rule, err := historicalRule(state, path, history)
		if err == nil && rule.Kind == "" {
			rule, err = sensitiveRule(files, state, path, history[path])
		}
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// historicalRule decides a rule from snapshot history alone. It returns an
// empty Kind when the checkout must decide.
func historicalRule(state selection.State, path string, history map[string]bool) (secureblob.PathRule, error) {
	for _, rule := range state.Include {
		if rule.Path == path {
			return secureblob.PathRule{}, nil
		}
	}
	for recorded := range history {
		if strings.HasPrefix(recorded, path+"/") {
			return secureblob.PathRule{Path: path, Kind: "directory"}, nil
		}
	}
	return secureblob.PathRule{}, nil
}

func sensitiveRule(files *storage.Dir, state selection.State, path string, recorded bool) (secureblob.PathRule, error) {
	for _, rule := range state.Include {
		if rule.Path == path {
			return secureblob.PathRule{Path: path, Kind: rule.Kind}, nil
		}
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		entries, err := files.ReadDir(filepath.FromSlash(strings.Join(parts[:i], "/")))
		if err != nil {
			return secureblob.PathRule{}, fmt.Errorf("%w: cannot inspect sensitive path %q: %v", storage.ErrConflict, path, err)
		}
		found, alias := false, ""
		for _, entry := range entries {
			if entry.Name() == part {
				found = true
				break
			}
			if storage.AliasKey(entry.Name()) == storage.AliasKey(part) {
				alias = entry.Name()
			}
		}
		if !found && alias != "" {
			return secureblob.PathRule{}, fmt.Errorf("%w: sensitive path %q is spelled %q on disk; use the exact spelling", storage.ErrConflict, path, strings.Join(append(parts[:i:i], alias), "/"))
		}
		if !found && recorded {
			// Deleted since it was saved: history holds it as a file.
			return secureblob.PathRule{Path: path, Kind: "file"}, nil
		}
		if !found {
			return secureblob.PathRule{}, fmt.Errorf("%w: sensitive path %q does not exist; create it before enabling encryption", storage.ErrConflict, path)
		}
	}
	info, err := files.Lstat(filepath.FromSlash(path))
	if err != nil {
		return secureblob.PathRule{}, fmt.Errorf("%w: cannot inspect sensitive path %q: %v", storage.ErrConflict, path, err)
	}
	switch {
	case info.IsDir():
		return secureblob.PathRule{Path: path, Kind: "directory"}, nil
	case info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0:
		return secureblob.PathRule{Path: path, Kind: "file"}, nil
	}
	return secureblob.PathRule{}, fmt.Errorf("%w: sensitive path %q is not a regular file, symlink or directory", storage.ErrConflict, path)
}
