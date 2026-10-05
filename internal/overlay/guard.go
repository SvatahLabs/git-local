package overlay

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"gitlocal/internal/storage"
)

// HasAny reports whether this repository has an overlay registration. Doctor
// uses it to require commit and push hooks even when no local-only paths exist.
func HasAny(ctx context.Context, commonPath string) (bool, error) {
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return false, err
	}
	defer common.Close()
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return false, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return false, err
	}
	defer d.Close()
	entries, err := d.ReadDir("overlays")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !storage.ValidUUID(entry.Name()) {
			return false, errors.New("invalid overlay workspace directory")
		}
		names, err := d.ReadDir(filepath.Join("overlays", entry.Name()))
		if err != nil {
			return false, err
		}
		for _, named := range names {
			if !named.IsDir() || !validName.MatchString(named.Name()) {
				return false, errors.New("invalid overlay registration")
			}
			var m Manifest
			if err := d.ReadJSON(filepath.Join(prefix(entry.Name(), named.Name()), "manifest.json"), &m); err != nil {
				if os.IsNotExist(err) {
					return true, nil // A pending installation still needs the guard.
				}
				return false, err
			}
			if err := checkManifest(m, entry.Name(), named.Name()); err != nil {
				return false, err
			}
			if m.State != "removed" {
				return true, nil
			}
		}
	}
	return false, nil
}

// LoadForPaths reads active patch versions for changed paths. An empty owner
// selects every workspace, including retained workspaces. The guard only opens
// version content for paths that Git says changed.
func LoadForPaths(ctx context.Context, commonPath, owner string, paths []string) ([]Version, error) {
	if owner != "" && !storage.ValidUUID(owner) {
		return nil, errors.New("invalid overlay workspace")
	}
	wanted := make(map[string]bool, len(paths))
	for _, path := range paths {
		if err := storage.ValidateSelectionPath(path); err != nil {
			return nil, err
		}
		wanted[storage.AliasKey(path)] = true
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	owners, err := d.ReadDir("overlays")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := []Version{}
	for _, entry := range owners {
		id := entry.Name()
		if !entry.IsDir() || !storage.ValidUUID(id) {
			return nil, errors.New("invalid overlay workspace directory")
		}
		if owner != "" && id != owner {
			continue
		}
		names, err := d.ReadDir(filepath.Join("overlays", id))
		if err != nil {
			return nil, err
		}
		for _, named := range names {
			name := named.Name()
			if !named.IsDir() || !validName.MatchString(name) {
				return nil, errors.New("invalid overlay registration")
			}
			var m Manifest
			if err := d.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &m); err != nil {
				if os.IsNotExist(err) {
					var pending pendingOverlay
					if err := d.ReadJSON(filepath.Join(prefix(id, name), "pending.json"), &pending); err != nil {
						return nil, err
					}
					if pending.WorkspaceID != id || pending.Name != name || storage.ValidateSelectionPath(pending.Path) != nil {
						return nil, errors.New("invalid pending overlay")
					}
					if wanted[storage.AliasKey(pending.Path)] {
						return nil, errors.New("overlay registration is pending")
					}
					continue
				}
				return nil, err
			}
			if err := checkManifest(m, id, name); err != nil {
				return nil, err
			}
			if m.State == "removed" {
				continue
			}
			if m.State == "removing" && wanted[storage.AliasKey(m.Path)] {
				return nil, errors.New("overlay removal is pending")
			}
			if wanted[storage.AliasKey(m.Path)] {
				v, err := readVersion(d, m, m.Version)
				if err != nil {
					return nil, err
				}
				result = append(result, v)
			}
		}
	}
	return result, nil
}

// ContainsPatch returns true when applying the saved patch changes nothing:
// the candidate already carries that patch. A conflicting candidate must be
// resolved before a guard can certify that it is clean.
func ContainsPatch(ctx context.Context, git string, v Version, candidate []byte) (bool, error) {
	if err := validateText(candidate); err != nil {
		return false, err
	}
	applied, err := Apply(ctx, git, v.Base, v.Local, candidate)
	if err != nil {
		return false, err
	}
	return bytes.Equal(applied, candidate), nil
}
