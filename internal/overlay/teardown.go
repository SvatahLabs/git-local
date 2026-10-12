package overlay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitlocal/internal/storage"
)

// Registered lists every overlay of every workspace that isn't removed,
// including one whose removal was interrupted ("removing"). An overlay whose
// add was interrupted (no manifest yet) is an error naming it.
func Registered(ctx context.Context, commonPath string) ([]Version, error) {
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
	var result []Version
	for _, owner := range owners {
		id := owner.Name()
		if !owner.IsDir() || !storage.ValidUUID(id) {
			return nil, errors.New("invalid overlay workspace directory")
		}
		entries, err := d.ReadDir(filepath.Join("overlays", id))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || !validName.MatchString(name) {
				return nil, errors.New("invalid overlay directory")
			}
			var m Manifest
			if err := d.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &m); os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: adding overlay %s of workspace %s was interrupted; run git local overlay add again or remove %s", storage.ErrConflict, name, id, filepath.Join(commonPath, "gitlocal", prefix(id, name)))
			} else if err != nil {
				return nil, err
			}
			if err := checkManifest(m, id, name); err != nil {
				return nil, err
			}
			if m.State == "removed" {
				continue
			}
			v, err := readVersion(d, m, m.Version)
			if err != nil {
				return nil, err
			}
			result = append(result, v)
		}
	}
	return result, nil
}
