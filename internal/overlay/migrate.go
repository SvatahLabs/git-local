package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
)

// Migrate repairs the shared filter installation of active overlays without
// creating an overlay version: it rewrites an absolute include from an earlier
// release (including one left by a moved checkout), recreates a missing
// include or filter configuration, and adds the direct required key. It then
// checks every managed path with git check-attr and undoes its changes if Git
// doesn't apply the filter. It returns what it changed. executable is used
// only when the filter configuration file is missing.
func Migrate(ctx context.Context, r *gitexec.Runner, cwd, commonPath, executable string) ([]string, error) {
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	paths, _, err := activePathsLocked(d, "", "", "", false)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	if _, exists, err := loadInstallation(d); err != nil {
		return nil, err
	} else if !exists {
		return nil, errors.New("overlay installation provenance is missing; cannot safely repair the filter")
	}
	includes, err := localIncludes(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	set := classifyIncludes(includes, commonPath)
	if len(set.foreign) > 0 {
		return nil, foreignIncludeError(set.foreign[0])
	}
	if set.count() > 1 {
		return nil, errors.New("duplicate overlay filter include")
	}
	requiredExists, err := localRequired(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	oldConfig, configExists, configMode, err := readOptional(d, filterConfigName, 1<<20)
	if err != nil {
		return nil, err
	}
	command := ""
	var newConfig []byte
	if configExists {
		var e error
		if _, command, e = installedCommand(oldConfig); e != nil {
			return nil, e
		}
	} else {
		if executable, err = filepath.EvalSymlinks(executable); err != nil {
			return nil, err
		}
		if newConfig, err = FilterConfig(executable); err != nil {
			return nil, err
		}
		command = filterCommandForOS(executable, runtime.GOOS)
	}
	legacy := set.legacy()
	if configExists && set.relative == 1 && requiredExists {
		return nil, nil
	}
	changed := []string{}
	migrated, includeAdded, requiredAdded, configWritten := "", false, false, false
	rollback := func(cause error) ([]string, error) {
		var recovery error
		if requiredAdded {
			recovery = errors.Join(recovery, unsetLocal(ctx, r, cwd, requiredKey))
		}
		if migrated != "" {
			_, e := localConfig(ctx, r, cwd, "--fixed-value", "--replace-all", "include.path", migrated, filterInclude)
			recovery = errors.Join(recovery, e)
		}
		if includeAdded {
			recovery = errors.Join(recovery, unsetLocal(ctx, r, cwd, "--fixed-value", "include.path", filterInclude))
		}
		if configWritten {
			recovery = errors.Join(recovery, restoreOptional(d, filterConfigName, oldConfig, configExists, configMode))
		}
		return nil, errors.Join(cause, recovery)
	}
	if !configExists {
		if err := d.AtomicWrite(filterConfigName, newConfig, 0600); err != nil {
			return rollback(err)
		}
		configWritten = true
		changed = append(changed, "recreated the filter configuration "+filterConfigName)
	}
	switch {
	case len(legacy) == 1:
		if _, err := localConfig(ctx, r, cwd, "--fixed-value", "--replace-all", "include.path", filterInclude, legacy[0]); err != nil {
			return rollback(err)
		}
		migrated = legacy[0]
		changed = append(changed, "rewrote the absolute filter include "+legacy[0]+" as "+filterInclude)
	case set.relative == 0:
		if _, err := localConfig(ctx, r, cwd, "--add", "include.path", filterInclude); err != nil {
			return rollback(err)
		}
		includeAdded = true
		changed = append(changed, "restored the filter include "+filterInclude)
	}
	if !requiredExists {
		if _, err := localConfig(ctx, r, cwd, requiredKey, "true"); err != nil {
			return rollback(err)
		}
		requiredAdded = true
		changed = append(changed, "set "+requiredKey+"=true in the local Git config")
	}
	if err := checkEffectiveFilter(ctx, r, cwd, command); err != nil {
		return rollback(err)
	}
	for _, path := range paths {
		attrs, err := r.CheckAttributes(ctx, cwd, path)
		if err != nil {
			return rollback(err)
		}
		if attrs["filter"] != "gitlocal-overlay" {
			return rollback(fmt.Errorf("Git does not apply the overlay filter to %q (filter: %s)", path, attrs["filter"]))
		}
	}
	if configWritten {
		if data, err := d.ReadFile(filterConfigName, 1<<20); err != nil || !bytes.Equal(data, newConfig) {
			return rollback(fmt.Errorf("filter configuration changed during repair: %v", err))
		}
	}
	return changed, nil
}
