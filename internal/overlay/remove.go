package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
)

// Remove deactivates one named overlay without changing its working file or
// deleting immutable versions. A "removing" manifest keeps the filter closed
// if the shared attributes/config update is interrupted; rerunning resumes it.
func Remove(ctx context.Context, r *gitexec.Runner, cwd, name string) (Manifest, error) {
	var empty Manifest
	if !validName.MatchString(name) || name == "." || name == ".." {
		return empty, errors.New("invalid overlay name")
	}
	p, err := NewProcessor(ctx, r, cwd, "")
	if err != nil {
		return empty, err
	}
	if p.WorkspaceID == "" {
		return empty, errors.New("worktree needs git local init before removing an overlay")
	}
	common, err := storage.OpenDir(p.Common)
	if err != nil {
		return empty, err
	}
	defer common.Close()
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(p.Common, "gitlocal"))
	if err != nil {
		return empty, err
	}
	defer d.Close()
	manifestPath := filepath.Join(prefix(p.WorkspaceID, name), "manifest.json")
	var m Manifest
	if err := d.ReadJSON(manifestPath, &m); err != nil {
		return empty, err
	}
	if err := checkManifest(m, p.WorkspaceID, name); err != nil {
		return empty, err
	}
	if m.State == "removed" {
		return m, nil
	}
	if _, err := readVersion(d, m, m.Version); err != nil {
		return empty, err
	}
	paths, pending, err := activePathsLocked(d, p.WorkspaceID, name, m.Path, true)
	if err != nil {
		return empty, err
	}
	if pending {
		return empty, errors.New("overlay installation needs recovery before removal")
	}
	remaining := make([]string, 0, len(paths))
	removed := false
	for _, path := range paths {
		if !removed && path == m.Path {
			removed = true
			continue
		}
		remaining = append(remaining, path)
	}
	if !removed {
		return empty, errors.New("overlay is absent from active attributes")
	}
	oldAttr, attrExists, attrMode, err := readOptional(common, "info/attributes", 1<<20)
	if err != nil {
		return empty, err
	}
	userAttr, err := AttributeUserBytes(oldAttr, paths)
	attrChanged := err == nil
	if err != nil && m.State == "removing" {
		userAttr, err = AttributeUserBytes(oldAttr, remaining)
	}
	if err != nil {
		return empty, err
	}
	newAttr, err := RenderAttributes(userAttr, remaining)
	if err != nil {
		return empty, err
	}
	install, installExists, err := loadInstallation(d)
	if err != nil {
		return empty, err
	}
	if len(remaining) == 0 && !installExists {
		return empty, errors.New("overlay installation provenance is missing; cannot safely restore Git attributes and config")
	}
	includes, err := localIncludes(ctx, r, p.Root)
	if err != nil {
		return empty, err
	}
	includeCount, legacy := managedIncludes(includes)
	if includeCount+len(legacy) > 1 {
		return empty, errors.New("duplicate overlay filter include")
	}
	if len(remaining) == 0 && install.IncludePathExisted && includeCount+len(legacy) != 1 {
		return empty, errors.New("preexisting overlay filter include was removed")
	}
	requiredExists, err := localRequired(ctx, r, p.Root)
	if err != nil {
		return empty, err
	}
	if m.State != "removing" {
		m.State = "removing"
		if err := d.WriteJSON(manifestPath, m); err != nil {
			return empty, err
		}
	}
	if attrChanged {
		if len(remaining) == 0 && !install.AttributesExisted && len(userAttr) == 0 {
			if err := common.Remove("info/attributes"); err != nil && !os.IsNotExist(err) {
				return empty, err
			}
		} else {
			mode := attrMode
			if len(remaining) == 0 && install.AttributesExisted {
				mode = os.FileMode(install.AttributesMode)
			}
			if !attrExists {
				mode = 0600
			}
			if err := common.AtomicWrite("info/attributes", newAttr, mode); err != nil {
				return empty, err
			}
		}
	} else if !bytes.Equal(oldAttr, newAttr) {
		return empty, errors.New("overlay attributes changed during removal")
	}
	if len(remaining) == 0 {
		if !install.IncludePathExisted {
			for _, include := range append([]string{filterInclude}, legacy...) {
				if err := unsetLocal(ctx, r, p.Root, "--fixed-value", "include.path", include); err != nil {
					return empty, err
				}
			}
		}
		if !install.RequiredExisted {
			if err := unsetLocal(ctx, r, p.Root, requiredKey); err != nil {
				return empty, err
			}
		}
		if err := restoreOptional(d, filterConfigName, install.ConfigData, install.ConfigExisted, os.FileMode(install.ConfigMode)); err != nil {
			return empty, fmt.Errorf("restore overlay filter config: %w", err)
		}
	} else {
		// Migrate an absolute include from an earlier release for the overlays
		// that stay active.
		if len(legacy) == 1 {
			if _, err := localConfig(ctx, r, p.Root, "--fixed-value", "--replace-all", "include.path", filterInclude, legacy[0]); err != nil {
				return empty, err
			}
		}
		if !requiredExists {
			if _, err := localConfig(ctx, r, p.Root, requiredKey, "true"); err != nil {
				return empty, err
			}
		}
	}
	m.State = "removed"
	if err := d.WriteJSON(manifestPath, m); err != nil {
		return empty, err
	}
	return m, nil
}
