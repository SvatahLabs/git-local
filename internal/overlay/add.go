package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func repositoryPath(root, cwd, given string) (string, error) {
	if given == "" || filepath.IsAbs(given) || strings.ContainsRune(given, 0) {
		return "", errors.New("overlay path must be relative to this checkout")
	}
	abs, err := filepath.Abs(filepath.Join(cwd, given))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("overlay path is outside this checkout")
	}
	rel = filepath.ToSlash(rel)
	if err := storage.ValidateSelectionPath(rel); err != nil {
		return "", err
	}
	return rel, nil
}

func localConfig(ctx context.Context, r *gitexec.Runner, root string, args ...string) ([]byte, error) {
	return gitConfig(ctx, r, root, true, args...)
}
func effectiveConfig(ctx context.Context, r *gitexec.Runner, root string, args ...string) ([]byte, error) {
	return gitConfig(ctx, r, root, false, args...)
}
func gitConfig(ctx context.Context, r *gitexec.Runner, root string, local bool, args ...string) ([]byte, error) {
	command := []string{"config"}
	if local {
		command = append(command, "--local")
	}
	cmd := exec.CommandContext(ctx, r.Executable(), append(command, args...)...)
	cmd.Dir = root
	cmd.WaitDelay = 2 * time.Second
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") && key != "LC_ALL" && key != "LANG" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("local Git configuration: %w", err)
	}
	return out, nil
}
func localIncludes(ctx context.Context, r *gitexec.Runner, root string) ([]string, error) {
	b, err := localConfig(ctx, r, root, "--null", "--get-all", "include.path")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	if len(b) == 0 || b[len(b)-1] != 0 {
		return nil, errors.New("malformed local include.path output")
	}
	parts := bytes.Split(b[:len(b)-1], []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, string(part))
	}
	return out, nil
}
func readOptional(d *storage.Dir, name string, limit int64) ([]byte, bool, os.FileMode, error) {
	data, err := d.ReadFile(name, limit)
	if os.IsNotExist(err) {
		return nil, false, 0, nil
	}
	if err != nil {
		return nil, false, 0, err
	}
	info, err := d.Lstat(name)
	if err != nil {
		return nil, false, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, 0, errors.New("extension metadata is not a regular file")
	}
	return data, true, info.Mode().Perm(), nil
}
func restoreOptional(d *storage.Dir, name string, data []byte, exists bool, mode os.FileMode) error {
	if exists {
		return d.AtomicWrite(name, data, mode)
	}
	err := d.Remove(name)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func activePathsLocked(d *storage.Dir, pendingID, pendingName, pendingPath string, allowRemoving bool) ([]string, bool, error) {
	owners, err := d.ReadDir("overlays")
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	paths := []string{}
	pendingFound := false
	for _, owner := range owners {
		id := owner.Name()
		if !owner.IsDir() || !storage.ValidUUID(id) {
			return nil, false, errors.New("invalid overlay workspace directory")
		}
		entries, err := d.ReadDir(filepath.Join("overlays", id))
		if err != nil {
			return nil, false, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || !validName.MatchString(name) {
				return nil, false, errors.New("invalid overlay directory")
			}
			var m Manifest
			e := d.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &m)
			if e == nil {
				if err := checkManifest(m, id, name); err != nil {
					return nil, false, err
				}
				if _, err := readVersion(d, m, m.Version); err != nil {
					return nil, false, err
				}
				if m.State == "removed" {
					continue
				}
				if m.State == "removing" {
					if !allowRemoving || id != pendingID || name != pendingName || m.Path != pendingPath {
						return nil, false, errors.New("overlay removal needs recovery")
					}
				}
				paths = append(paths, m.Path)
				continue
			}
			if !os.IsNotExist(e) {
				return nil, false, e
			}
			var pending pendingOverlay
			if err := d.ReadJSON(filepath.Join(prefix(id, name), "pending.json"), &pending); err != nil {
				return nil, false, err
			}
			if pending.WorkspaceID != id || pending.Name != name || storage.ValidateSelectionPath(pending.Path) != nil {
				return nil, false, errors.New("invalid pending overlay")
			}
			if id != pendingID || name != pendingName || pending.Path != pendingPath {
				return nil, false, errors.New("another overlay registration needs recovery")
			}
			pendingFound = true
		}
	}
	return paths, pendingFound, nil
}

// Add captures a complete local patch against the stage-zero blob. A pending
// record is published before shared attributes so interrupted registration
// fails closed; the active version pointer is published last.
func Add(ctx context.Context, r *gitexec.Runner, cwd, name, given, executable string) (Manifest, error) {
	var empty Manifest
	if !validName.MatchString(name) || name == "." || name == ".." {
		return empty, errors.New("invalid overlay name")
	}
	p, err := NewProcessor(ctx, r, cwd, "")
	if err != nil {
		return empty, err
	}
	if p.WorkspaceID == "" {
		return empty, errors.New("worktree needs git local init before adding an overlay")
	}
	path, err := repositoryPath(p.Root, cwd, given)
	if err != nil {
		return empty, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return empty, err
	}
	filterData, err := FilterConfig(executable)
	if err != nil {
		return empty, err
	}
	base, err := r.IndexBlob(ctx, p.Root, "", path)
	if err != nil {
		return empty, fmt.Errorf("overlay requires a tracked index blob: %w", err)
	}
	mode, err := r.IndexMode(ctx, p.Root, "", path)
	if err != nil {
		return empty, err
	}
	if mode != "100644" && mode != "100755" {
		return empty, errors.New("overlay requires a tracked regular file")
	}
	files, err := storage.OpenDir(p.Root)
	if err != nil {
		return empty, err
	}
	defer files.Close()
	local, err := files.ReadFile(filepath.FromSlash(path), MaxText+1)
	if err != nil {
		return empty, err
	}
	info, err := files.Lstat(filepath.FromSlash(path))
	if err != nil {
		return empty, err
	}
	if !info.Mode().IsRegular() {
		return empty, errors.New("overlay working path is not a regular file")
	}
	if err := validateText(base); err != nil {
		return empty, err
	}
	if err := validateText(local); err != nil {
		return empty, err
	}
	if bytes.Equal(base, local) {
		return empty, errors.New("working file has no local patch to save")
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
	var record workspace.Record
	if err := d.ReadJSON(filepath.Join("workspaces", p.WorkspaceID, "workspace.json"), &record); err != nil {
		return empty, err
	}
	if record.State != "active" || record.WorkspaceID != p.WorkspaceID {
		return empty, errors.New("overlay workspace is not active")
	}
	currentBase, err := r.IndexBlob(ctx, p.Root, "", path)
	if err != nil {
		return empty, err
	}
	currentLocal, err := files.ReadFile(filepath.FromSlash(path), MaxText+1)
	if err != nil {
		return empty, err
	}
	if !bytes.Equal(base, currentBase) || !bytes.Equal(local, currentLocal) {
		return empty, errors.New("overlay source changed during registration")
	}
	paths, pendingFound, err := activePathsLocked(d, p.WorkspaceID, name, path, false)
	if err != nil {
		return empty, err
	}
	var existing Manifest
	existingErr := d.ReadJSON(filepath.Join(prefix(p.WorkspaceID, name), "manifest.json"), &existing)
	if existingErr == nil {
		if err := checkManifest(existing, p.WorkspaceID, name); err != nil {
			return empty, err
		}
		if existing.Path != path {
			return empty, errors.New("overlay name is assigned to another path")
		}
		if _, err := readVersion(d, existing, existing.Version); err != nil {
			return empty, err
		}
		if existing.State == "removed" {
			if err := ensureNoSiblingOwner(d, p.WorkspaceID, name, path); err != nil {
				return empty, err
			}
		}
	} else if !os.IsNotExist(existingErr) {
		return empty, existingErr
	} else if err := ensureNoSiblingOwner(d, p.WorkspaceID, name, path); err != nil {
		return empty, err
	}
	if err := common.MkdirAll("info"); err != nil {
		return empty, err
	}
	oldAttr, attrExists, attrMode, err := readOptional(common, "info/attributes", 1<<20)
	if err != nil {
		return empty, err
	}
	userAttr, err := AttributeUserBytes(oldAttr, paths)
	if err != nil && pendingFound {
		userAttr, err = AttributeUserBytes(oldAttr, append(append([]string{}, paths...), path))
	}
	if err != nil {
		return empty, err
	}
	newPaths := append(append([]string{}, paths...), path)
	newAttr, err := RenderAttributes(userAttr, newPaths)
	if err != nil {
		return empty, err
	}
	attrs, err := r.CheckAttributes(ctx, p.Root, path)
	if err != nil {
		return empty, err
	}
	if value := attrs["filter"]; value != "unspecified" && value != "gitlocal-overlay" {
		return empty, fmt.Errorf("existing filter %q cannot be composed with overlay", value)
	}
	for _, key := range []string{"text", "eol", "ident"} {
		if value := attrs[key]; value != "unspecified" && value != "unset" {
			return empty, fmt.Errorf("existing %s attribute requires a separate conversion policy", key)
		}
	}
	includes, err := localIncludes(ctx, r, p.Root)
	if err != nil {
		return empty, err
	}
	includeCount, legacy := managedIncludes(includes)
	if includeCount+len(legacy) > 1 {
		return empty, errors.New("duplicate overlay filter include")
	}
	if len(paths) > 0 && includeCount+len(legacy) == 0 {
		return empty, errors.New("active overlays have no required filter configuration")
	}
	requiredExisted, err := localRequired(ctx, r, p.Root)
	if err != nil {
		return empty, err
	}
	oldConfig, configExists, configMode, err := readOptional(d, filterConfigName, 1<<20)
	if err != nil {
		return empty, err
	}
	if len(paths) > 0 && !configExists {
		return empty, errors.New("active overlays have no filter configuration file")
	}
	if configExists {
		if len(paths) > 0 {
			_, command, err := installedCommand(oldConfig)
			if err != nil {
				return empty, err
			}
			// An absolute include from an earlier release may no longer
			// resolve; the effective filter is checked after migration.
			if len(legacy) == 0 {
				if err := checkEffectiveFilter(ctx, r, p.Root, command); err != nil {
					return empty, err
				}
			}
		} else if !bytes.HasPrefix(oldConfig, []byte("[filter \"gitlocal-overlay\"]\n")) {
			return empty, errors.New("overlay filter configuration was changed")
		}
	}
	_, installExists, err := loadInstallation(d)
	if err != nil {
		return empty, err
	}
	if !pendingFound {
		if err := d.MkdirAll(prefix(p.WorkspaceID, name)); err != nil {
			return empty, err
		}
		if err := d.WriteJSON(filepath.Join(prefix(p.WorkspaceID, name), "pending.json"), pendingOverlay{p.WorkspaceID, name, path}); err != nil {
			return empty, err
		}
	}
	if len(paths) == 0 && (!installExists || !pendingFound) {
		record := installation{SchemaVersion: 1, AttributesExisted: attrExists, AttributesMode: uint32(attrMode), ConfigExisted: configExists, ConfigMode: uint32(configMode), ConfigData: bytes.Clone(oldConfig), IncludePathExisted: includeCount+len(legacy) == 1, RequiredExisted: requiredExisted}
		if err := d.WriteJSON(installationFile, record); err != nil {
			return empty, err
		}
	}
	configWritten, includeAdded, requiredAdded, attrWritten := false, false, false, false
	migrated := ""
	rollback := func(cause error) (Manifest, error) {
		var recovery error
		if attrWritten {
			recovery = errors.Join(recovery, restoreOptional(common, "info/attributes", oldAttr, attrExists, attrMode))
		}
		if requiredAdded {
			recovery = errors.Join(recovery, unsetLocal(ctx, r, p.Root, requiredKey))
		}
		if migrated != "" {
			_, e := localConfig(ctx, r, p.Root, "--fixed-value", "--replace-all", "include.path", migrated, filterInclude)
			recovery = errors.Join(recovery, e)
		}
		if includeAdded {
			recovery = errors.Join(recovery, unsetLocal(ctx, r, p.Root, "--fixed-value", "include.path", filterInclude))
		}
		if configWritten {
			recovery = errors.Join(recovery, restoreOptional(d, filterConfigName, oldConfig, configExists, configMode))
		}
		return empty, errors.Join(cause, recovery)
	}
	if err := d.AtomicWrite(filterConfigName, filterData, 0600); err != nil {
		return rollback(err)
	}
	configWritten = true
	if len(legacy) == 1 {
		if _, err := localConfig(ctx, r, p.Root, "--fixed-value", "--replace-all", "include.path", filterInclude, legacy[0]); err != nil {
			return rollback(err)
		}
		migrated = legacy[0]
	} else if includeCount == 0 {
		if _, err := localConfig(ctx, r, p.Root, "--add", "include.path", filterInclude); err != nil {
			return rollback(err)
		}
		includeAdded = true
	}
	if !requiredExisted {
		if _, err := localConfig(ctx, r, p.Root, requiredKey, "true"); err != nil {
			return rollback(err)
		}
		requiredAdded = true
	}
	if err := checkEffectiveFilter(ctx, r, p.Root, filterCommandForOS(executable, runtime.GOOS)); err != nil {
		return rollback(err)
	}
	modeToWrite := attrMode
	if !attrExists {
		modeToWrite = 0600
	}
	if err := common.AtomicWrite("info/attributes", newAttr, modeToWrite); err != nil {
		return rollback(err)
	}
	attrWritten = true
	m, err := publishVersionLocked(d, p.WorkspaceID, name, path, base, local)
	if err != nil {
		return rollback(err)
	}
	if err := d.Remove(filepath.Join(prefix(p.WorkspaceID, name), "pending.json")); err != nil && !os.IsNotExist(err) {
		return m, fmt.Errorf("overlay version %d was installed but pending marker cleanup failed: %w", m.Version, err)
	}
	return m, nil
}
