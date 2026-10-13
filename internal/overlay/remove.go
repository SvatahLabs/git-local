package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// Remove deactivates one named overlay without changing its working file or
// deleting immutable versions. A "removing" manifest keeps the filter closed
// if the shared attributes/config update is interrupted; rerunning resumes it.
func Remove(ctx context.Context, r *gitexec.Runner, cwd, name string) (Manifest, error) {
	return RemoveFor(ctx, r, cwd, "", name)
}

// RemoveFor removes an overlay of workspace owner, or of the current
// workspace when owner is empty. Another workspace's overlay can be removed
// only once that workspace is retained: its worktree is gone, so nothing else
// can remove it, and its attribute line, filter and hook requirement would
// otherwise stay forever.
func RemoveFor(ctx context.Context, r *gitexec.Runner, cwd, owner, name string) (Manifest, error) {
	m, _, err := removeFor(ctx, r, cwd, owner, name, false)
	return m, err
}

// RemoveForDeinit is RemoveFor for git local deinit. When the installation
// record is missing, the last overlay can't tell which Git attributes and
// config existed before overlays; RemoveFor refuses then, and nothing else
// could remove the filter. Deinit removes them as installed by git-local
// instead and returns a warning naming what it removed.
func RemoveForDeinit(ctx context.Context, r *gitexec.Runner, cwd, owner, name string) (Manifest, []string, error) {
	return removeFor(ctx, r, cwd, owner, name, true)
}

func removeFor(ctx context.Context, r *gitexec.Runner, cwd, owner, name string, assumeInstalled bool) (Manifest, []string, error) {
	m, warning, err := removeOverlay(ctx, r, cwd, owner, name, assumeInstalled)
	if warning == "" {
		return m, nil, err
	}
	return m, []string{warning}, err
}

func removeOverlay(ctx context.Context, r *gitexec.Runner, cwd, owner, name string, assumeInstalled bool) (Manifest, string, error) {
	var empty Manifest
	warning := ""
	if !validName.MatchString(name) || name == "." || name == ".." {
		return empty, "", errors.New("invalid overlay name")
	}
	p, err := NewProcessor(ctx, r, cwd, "")
	if err != nil {
		return empty, "", err
	}
	if p.WorkspaceID == "" {
		return empty, "", errors.New("worktree needs git local init before removing an overlay")
	}
	common, err := storage.OpenDir(p.Common)
	if err != nil {
		return empty, "", err
	}
	defer common.Close()
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return empty, "", err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(p.Common, "gitlocal"))
	if err != nil {
		return empty, "", err
	}
	defer d.Close()
	if owner == "" || owner == p.WorkspaceID {
		owner = p.WorkspaceID
	} else {
		if !storage.ValidUUID(owner) {
			return empty, "", errors.New("invalid workspace UUID")
		}
		var record workspace.Record
		if err := d.ReadJSON(filepath.Join("workspaces", owner, "workspace.json"), &record); err != nil {
			return empty, "", err
		}
		if record.State != "retained" {
			return empty, "", fmt.Errorf("%w: workspace %s is %s; remove its overlay from that worktree, or retire the workspace first", storage.ErrConflict, owner, record.State)
		}
	}
	manifestPath := filepath.Join(prefix(owner, name), "manifest.json")
	var m Manifest
	if err := d.ReadJSON(manifestPath, &m); err != nil {
		return empty, "", err
	}
	if err := checkManifest(m, owner, name); err != nil {
		return empty, "", err
	}
	if m.State == "removed" {
		return m, "", nil
	}
	if _, err := readVersion(d, m, m.Version); err != nil {
		return empty, "", err
	}
	paths, pending, err := activePathsLocked(d, owner, name, m.Path, true)
	if err != nil {
		return empty, "", err
	}
	if pending {
		return empty, "", errors.New("overlay installation needs recovery before removal")
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
		return empty, "", errors.New("overlay is absent from active attributes")
	}
	oldAttr, attrExists, attrMode, err := readOptional(common, "info/attributes", 1<<20)
	if err != nil {
		return empty, "", err
	}
	userAttr, err := AttributeUserBytes(oldAttr, paths)
	attrChanged := err == nil
	if err != nil && m.State == "removing" {
		userAttr, err = AttributeUserBytes(oldAttr, remaining)
	}
	if err != nil {
		return empty, "", err
	}
	newAttr, err := RenderAttributes(userAttr, remaining)
	if err != nil {
		return empty, "", err
	}
	install, installExists, err := loadInstallation(d)
	if err != nil {
		return empty, "", err
	}
	if len(remaining) == 0 && !installExists {
		if !assumeInstalled {
			return empty, "", errors.New("overlay installation provenance is missing; cannot safely restore Git attributes and config")
		}
		install = installation{SchemaVersion: 1}
		warning = fmt.Sprintf("the overlay installation record %s was missing, so the overlay filter (include.path %s, %s, its config file and the managed block in info/attributes) was removed as installed by git-local; add back any of them you had set yourself", filepath.Join(p.Common, "gitlocal", installationFile), filterInclude, requiredKey)
	}
	if len(remaining) == 0 && assumeInstalled && (install.IncludePathExisted || install.RequiredExisted) {
		// The include names a file in the private state, which deinit sets
		// aside, and deinit's final check refuses while either is left.
		install.IncludePathExisted, install.RequiredExisted = false, false
		warning = fmt.Sprintf("include.path %s and %s were set before overlays were installed; deinit removed them with the overlay filter", filterInclude, requiredKey)
	}
	includes, err := localIncludes(ctx, r, p.Root)
	if err != nil {
		return empty, "", err
	}
	set := classifyIncludes(includes, p.Common)
	if len(set.foreign) > 0 {
		return empty, "", foreignIncludeError(set.foreign[0])
	}
	includeCount, legacy := set.relative, set.legacy()
	if includeCount+len(legacy) > 1 {
		return empty, "", errors.New("duplicate overlay filter include")
	}
	if len(remaining) == 0 && install.IncludePathExisted && includeCount+len(legacy) != 1 {
		return empty, "", errors.New("preexisting overlay filter include was removed")
	}
	requiredExists, err := localRequired(ctx, r, p.Root)
	if err != nil {
		return empty, "", err
	}
	if m.State != "removing" {
		m.State = "removing"
		if err := d.WriteJSON(manifestPath, m); err != nil {
			return empty, "", err
		}
	}
	if attrChanged {
		if len(remaining) == 0 && !install.AttributesExisted && len(userAttr) == 0 {
			if err := common.Remove("info/attributes"); err != nil && !os.IsNotExist(err) {
				return empty, "", err
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
				return empty, "", err
			}
		}
	} else if !bytes.Equal(oldAttr, newAttr) {
		return empty, "", errors.New("overlay attributes changed during removal")
	}
	if len(remaining) == 0 {
		if !install.IncludePathExisted {
			for _, include := range append([]string{filterInclude}, legacy...) {
				if err := unsetLocal(ctx, r, p.Root, "--fixed-value", "include.path", include); err != nil {
					return empty, "", err
				}
			}
		}
		if !install.RequiredExisted {
			if err := unsetLocal(ctx, r, p.Root, requiredKey); err != nil {
				return empty, "", err
			}
		}
		if err := restoreOptional(d, filterConfigName, install.ConfigData, install.ConfigExisted, os.FileMode(install.ConfigMode)); err != nil {
			return empty, "", fmt.Errorf("restore overlay filter config: %w", err)
		}
	} else {
		// Migrate an absolute include from an earlier release for the overlays
		// that stay active.
		if len(legacy) == 1 {
			if _, err := localConfig(ctx, r, p.Root, "--fixed-value", "--replace-all", "include.path", filterInclude, legacy[0]); err != nil {
				return empty, "", err
			}
		}
		if !requiredExists {
			if _, err := localConfig(ctx, r, p.Root, requiredKey, "true"); err != nil {
				return empty, "", err
			}
		}
	}
	// While the filter ran, Git may have cached the working file's size and
	// time next to the upstream blob (after git add, or a status when the
	// patch kept the size). Without the filter it would trust that cache,
	// show the local content as unchanged and let a checkout overwrite it, so
	// the file's times are renewed for Git to read it again.
	if owner == p.WorkspaceID {
		if err := renewTimes(p.Root, m.Path); err != nil {
			return empty, "", err
		}
	}
	m.State = "removed"
	if err := d.WriteJSON(manifestPath, m); err != nil {
		return empty, "", err
	}
	return m, warning, nil
}

// renewTimes sets a working file's access and modification times to now,
// which also changes its inode change time.
func renewTimes(root, path string) error {
	files, err := storage.OpenDir(root)
	if err != nil {
		return err
	}
	defer files.Close()
	info, err := files.Lstat(filepath.FromSlash(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	now := time.Now()
	return os.Chtimes(filepath.Join(root, filepath.FromSlash(path)), now, now)
}
