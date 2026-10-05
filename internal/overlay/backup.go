package overlay

import (
	"errors"
	"os"
	"path/filepath"

	"gitlocal/internal/storage"
)

// ValidateBackupRecords verifies the saved patches before an encrypted
// archive is published or adopted. A pending add/remove is not a stable
// snapshot, even when the archive itself is authenticated.
func ValidateBackupRecords(state *storage.Dir) ([]Manifest, error) {
	owners, err := state.ReadDir("overlays")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifests []Manifest
	for _, owner := range owners {
		id := owner.Name()
		if !owner.IsDir() || !storage.ValidUUID(id) {
			return nil, errors.New("invalid backed-up overlay workspace")
		}
		names, err := state.ReadDir(filepath.Join("overlays", id))
		if err != nil {
			return nil, err
		}
		for _, named := range names {
			name := named.Name()
			if !named.IsDir() || !validName.MatchString(name) || name == "." || name == ".." {
				return nil, errors.New("invalid backed-up overlay registration")
			}
			var manifest Manifest
			if err := state.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &manifest); err != nil {
				return nil, err
			}
			if err := checkManifest(manifest, id, name); err != nil {
				return nil, err
			}
			if manifest.State == "removing" {
				return nil, errors.New("overlay removal is pending; finish it before backing up")
			}
			if _, err := state.Lstat(filepath.Join(prefix(id, name), "pending.json")); err == nil {
				return nil, errors.New("overlay registration is pending; finish it before backing up")
			} else if !os.IsNotExist(err) {
				return nil, err
			}
			for version := uint32(1); version <= manifest.Version; version++ {
				if _, err := readVersion(state, manifest, version); err != nil {
					return nil, err
				}
			}
			manifests = append(manifests, manifest)
		}
	}
	return manifests, nil
}

// ArchiveForAdoption keeps every immutable patch version but makes active
// registrations historical. The original filter command and attributes point
// at the source machine, while the fresh clone has no overlay working bytes.
func ArchiveForAdoption(state *storage.Dir) error {
	manifests, err := ValidateBackupRecords(state)
	if err != nil {
		return err
	}
	for _, manifest := range manifests {
		if manifest.State == "removed" {
			continue
		}
		manifest.State = "removed"
		if err := state.WriteJSON(filepath.Join(prefix(manifest.WorkspaceID, manifest.Name), "manifest.json"), manifest); err != nil {
			return err
		}
	}
	return nil
}
