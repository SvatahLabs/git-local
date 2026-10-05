package overlay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitlocal/internal/storage"
)

const recordSchema = 1

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	WorkspaceID   string `json:"workspaceId"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	State         string `json:"state,omitempty"`
	Version       uint32 `json:"version"`
	BaseSHA256    string `json:"baseSha256"`
	LocalSHA256   string `json:"localSha256"`
}

type Version struct {
	Manifest    Manifest
	Base, Local []byte
}

type pendingOverlay struct {
	WorkspaceID string `json:"workspaceId"`
	Name        string `json:"name"`
	Path        string `json:"path"`
}

func digest(data []byte) string     { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func prefix(id, name string) string { return filepath.Join("overlays", id, name) }
func versionPath(id, name string, version uint32) string {
	return filepath.Join(prefix(id, name), fmt.Sprintf("v%06d", version))
}
func checkIdentity(id, name string) error {
	if !storage.ValidUUID(id) || !validName.MatchString(name) || name == "." || name == ".." {
		return errors.New("invalid overlay workspace or name")
	}
	return nil
}
func checkManifest(m Manifest, id, name string) error {
	if m.SchemaVersion != recordSchema || m.WorkspaceID != id || m.Name != name || (m.State != "" && m.State != "removing" && m.State != "removed") || m.Version == 0 || m.Version > 999999 || storage.ValidateSelectionPath(m.Path) != nil || strings.Contains(m.Path, "\\") || len(m.BaseSHA256) != 64 || len(m.LocalSHA256) != 64 {
		return errors.New("invalid overlay manifest")
	}
	return nil
}

func readVersion(d *storage.Dir, m Manifest, version uint32) (Version, error) {
	var v Version
	if version == 0 || version > m.Version {
		return v, errors.New("overlay version is unavailable")
	}
	var checks struct {
		BaseSHA256  string `json:"baseSha256"`
		LocalSHA256 string `json:"localSha256"`
	}
	basePath := versionPath(m.WorkspaceID, m.Name, version)
	if err := d.ReadJSON(filepath.Join(basePath, "checksums.json"), &checks); err != nil {
		return v, err
	}
	base, err := d.ReadFile(filepath.Join(basePath, "base"), MaxText+1)
	if err != nil {
		return v, err
	}
	local, err := d.ReadFile(filepath.Join(basePath, "local"), MaxText+1)
	if err != nil {
		return v, err
	}
	if err := validateText(base); err != nil {
		return v, err
	}
	if err := validateText(local); err != nil {
		return v, err
	}
	if digest(base) != checks.BaseSHA256 || digest(local) != checks.LocalSHA256 {
		return v, errors.New("overlay version checksum mismatch")
	}
	if version == m.Version && (checks.BaseSHA256 != m.BaseSHA256 || checks.LocalSHA256 != m.LocalSHA256) {
		return v, errors.New("active overlay checksum mismatch")
	}
	m.Version = version
	m.BaseSHA256 = checks.BaseSHA256
	m.LocalSHA256 = checks.LocalSHA256
	return Version{Manifest: m, Base: base, Local: local}, nil
}

// LoadVersion reads an immutable named patch version under the repository lock.
// version 0 selects the active version.
func LoadVersion(ctx context.Context, commonPath, id, name string, version uint32) (Version, error) {
	var empty Version
	if err := checkIdentity(id, name); err != nil {
		return empty, err
	}
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return empty, err
	}
	defer common.Close()
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return empty, err
	}
	defer d.Close()
	var m Manifest
	if err := d.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &m); err != nil {
		return empty, err
	}
	if err := checkManifest(m, id, name); err != nil {
		return empty, err
	}
	if version == 0 {
		version = m.Version
	}
	return readVersion(d, m, version)
}

// LoadPath finds this workspace's active overlay for one literal repository
// path. An attribute shared with another worktree is a pass-through here.
func LoadPath(ctx context.Context, commonPath, id, path string) (Version, bool, bool, error) {
	var empty Version
	if (id != "" && !storage.ValidUUID(id)) || storage.ValidateSelectionPath(path) != nil {
		return empty, false, false, errors.New("invalid overlay lookup")
	}
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return empty, false, false, err
	}
	defer common.Close()
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return empty, false, false, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return empty, false, false, err
	}
	defer d.Close()
	var entries []os.DirEntry
	if id != "" {
		entries, err = d.ReadDir(filepath.Join("overlays", id))
		if os.IsNotExist(err) {
			entries = nil
			err = nil
		}
		if err != nil {
			return empty, false, false, err
		}
	}
	var found *Manifest
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !validName.MatchString(name) {
			return empty, false, false, errors.New("invalid overlay registration")
		}
		var m Manifest
		if err := d.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &m); err != nil {
			if !os.IsNotExist(err) {
				return empty, false, false, err
			}
			var pending pendingOverlay
			if err := d.ReadJSON(filepath.Join(prefix(id, name), "pending.json"), &pending); err != nil {
				return empty, false, false, err
			}
			if pending.WorkspaceID != id || pending.Name != name || storage.ValidateSelectionPath(pending.Path) != nil {
				return empty, false, false, errors.New("invalid pending overlay")
			}
			if storage.AliasKey(pending.Path) == storage.AliasKey(path) {
				return empty, false, false, errors.New("overlay registration is pending; rerun git local overlay add")
			}
			continue
		}
		if err := checkManifest(m, id, name); err != nil {
			return empty, false, false, err
		}
		if storage.AliasKey(m.Path) == storage.AliasKey(path) {
			if m.State == "removed" {
				continue
			}
			if m.State == "removing" {
				return empty, false, false, errors.New("overlay removal is pending; rerun git local overlay remove")
			}
			if found != nil {
				return empty, false, false, errors.New("multiple overlays own one path")
			}
			found = &m
		}
	}
	if found == nil {
		owners, err := d.ReadDir("overlays")
		if os.IsNotExist(err) {
			return empty, false, false, nil
		}
		if err != nil {
			return empty, false, false, err
		}
		for _, owner := range owners {
			if owner.Name() == id {
				continue
			}
			if !owner.IsDir() || !storage.ValidUUID(owner.Name()) {
				return empty, false, false, errors.New("invalid overlay workspace registration")
			}
			names, err := d.ReadDir(filepath.Join("overlays", owner.Name()))
			if err != nil {
				return empty, false, false, err
			}
			for _, entry := range names {
				if !entry.IsDir() || !validName.MatchString(entry.Name()) {
					return empty, false, false, errors.New("invalid overlay registration")
				}
				var m Manifest
				if err := d.ReadJSON(filepath.Join(prefix(owner.Name(), entry.Name()), "manifest.json"), &m); err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return empty, false, false, err
				}
				if err := checkManifest(m, owner.Name(), entry.Name()); err != nil {
					return empty, false, false, err
				}
				if storage.AliasKey(m.Path) == storage.AliasKey(path) {
					if m.State == "removed" {
						continue
					}
					if m.State == "removing" {
						return empty, false, false, errors.New("overlay removal is pending; rerun git local overlay remove")
					}
					return empty, false, true, nil
				}
			}
		}
		return empty, false, false, nil
	}
	v, err := readVersion(d, *found, found.Version)
	return v, err == nil, true, err
}

// PublishVersion advances a named patch only after both immutable content files
// and their checksums are durable. Callers must validate tracked-file ownership
// and install the filter boundary before exposing an overlay to normal Git use.
func PublishVersion(ctx context.Context, commonPath, id, name, path string, base, local []byte) (Manifest, error) {
	var empty Manifest
	if err := checkIdentity(id, name); err != nil {
		return empty, err
	}
	if err := storage.ValidateSelectionPath(path); err != nil {
		return empty, err
	}
	if err := validateText(base); err != nil {
		return empty, err
	}
	if err := validateText(local); err != nil {
		return empty, err
	}
	if string(base) == string(local) {
		return empty, errors.New("overlay patch has no change")
	}
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return empty, err
	}
	defer common.Close()
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return empty, err
	}
	defer d.Close()
	return publishVersionLocked(d, id, name, path, base, local)
}

func publishVersionLocked(d *storage.Dir, id, name, path string, base, local []byte) (Manifest, error) {
	var empty Manifest
	if err := checkIdentity(id, name); err != nil {
		return empty, err
	}
	if err := storage.ValidateSelectionPath(path); err != nil {
		return empty, err
	}
	if err := validateText(base); err != nil {
		return empty, err
	}
	if err := validateText(local); err != nil {
		return empty, err
	}
	if string(base) == string(local) {
		return empty, errors.New("overlay patch has no change")
	}
	var m Manifest
	err := d.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &m)
	if err == nil {
		if err := checkManifest(m, id, name); err != nil {
			return empty, err
		}
		if m.Path != path {
			return empty, errors.New("overlay name is already assigned to a different path")
		}
		if m.Version == 999999 {
			return empty, errors.New("overlay version limit reached")
		}
		if _, err := readVersion(d, m, m.Version); err != nil {
			return empty, err
		}
		if m.State == "removing" {
			return empty, errors.New("overlay removal is pending")
		}
		if m.State == "removed" {
			if err := ensureNoSiblingOwner(d, id, name, path); err != nil {
				return empty, err
			}
		}
	} else if os.IsNotExist(err) {
		m = Manifest{SchemaVersion: recordSchema, WorkspaceID: id, Name: name, Path: path}
		if err := ensureNoSiblingOwner(d, id, name, path); err != nil {
			return empty, err
		}
	} else {
		return empty, err
	}
	m.Version++
	m.State = ""
	m.BaseSHA256 = digest(base)
	m.LocalSHA256 = digest(local)
	versionDir := versionPath(id, name, m.Version)
	if err := d.MkdirAll(versionDir); err != nil {
		return empty, err
	}
	if err := d.AtomicWrite(filepath.Join(versionDir, "base"), base, 0600); err != nil {
		return empty, err
	}
	if err := d.AtomicWrite(filepath.Join(versionDir, "local"), local, 0600); err != nil {
		return empty, err
	}
	checks := struct {
		BaseSHA256  string `json:"baseSha256"`
		LocalSHA256 string `json:"localSha256"`
	}{m.BaseSHA256, m.LocalSHA256}
	if err := d.WriteJSON(filepath.Join(versionDir, "checksums.json"), checks); err != nil {
		return empty, err
	}
	if err := d.WriteJSON(filepath.Join(prefix(id, name), "manifest.json"), m); err != nil {
		return empty, err
	}
	return m, nil
}

func ensureNoSiblingOwner(d *storage.Dir, id, name, path string) error {
	entries, err := d.ReadDir(filepath.Join("overlays", id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == name {
			continue
		}
		if !validName.MatchString(entry.Name()) || !entry.IsDir() {
			return errors.New("invalid overlay registration")
		}
		var other Manifest
		if err := d.ReadJSON(filepath.Join(prefix(id, entry.Name()), "manifest.json"), &other); err != nil {
			return err
		}
		if err := checkManifest(other, id, entry.Name()); err != nil {
			return err
		}
		if other.State == "removed" {
			continue
		}
		if storage.AliasKey(other.Path) == storage.AliasKey(path) {
			return errors.New("another active overlay owns this path")
		}
	}
	return nil
}
