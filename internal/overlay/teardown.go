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
	"gitlocal/internal/workspace"
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
	return RegisteredLocked(commonPath)
}

// RegisteredLocked is Registered for a caller that holds the repository lock.
func RegisteredLocked(commonPath string) ([]Version, error) {
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

// FilterLeftovers names what of git-local's overlay filter is installed in
// the repository at root: a local include.path of its filter config, the
// direct required key, and the managed info/attributes block. It takes no
// lock.
func FilterLeftovers(ctx context.Context, r *gitexec.Runner, root, commonPath string) ([]string, error) {
	var found []string
	includes, err := localIncludes(ctx, r, root)
	if err != nil {
		return nil, err
	}
	if set := classifyIncludes(includes, commonPath); set.count() > 0 {
		found = append(found, "include.path "+filterInclude)
	}
	required, err := localRequired(ctx, r, root)
	if err != nil && !required {
		return nil, err
	}
	if required {
		found = append(found, requiredKey)
	}
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	data, _, _, err := readOptional(common, "info/attributes", 1<<20)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(data, []byte(attrBegin)) || bytes.Contains(data, []byte(attrEnd)) {
		found = append(found, "the managed block in info/attributes")
	}
	return found, nil
}

// RemoveFilter removes git-local's overlay filter from the repository at cwd
// once no overlay is registered: the local include.path of its filter config,
// the direct required key and the managed info/attributes block. Removing the
// last overlay normally does this; it is left behind when an overlay add was
// interrupted before writing its manifest and its directory was then deleted,
// and Git then fails on that path ("clean filter failed"). It returns what it
// removed.
func RemoveFilter(ctx context.Context, r *gitexec.Runner, cwd string) ([]string, error) {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	versions, err := RegisteredLocked(w.Common)
	if err != nil {
		return nil, err
	}
	if len(versions) > 0 {
		return nil, fmt.Errorf("%w: overlay %s is still registered", storage.ErrConflict, versions[0].Manifest.Name)
	}
	found, err := FilterLeftovers(ctx, r, w.Root, w.Common)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	data, exists, mode, err := readOptional(common, "info/attributes", 1<<20)
	if err != nil {
		return nil, err
	}
	if begin, end := bytes.Index(data, []byte(attrBegin)), bytes.Index(data, []byte(attrEnd)); begin >= 0 || end >= 0 {
		if begin != 0 || end < 0 {
			return nil, fmt.Errorf("%w: the git-local block in %s was edited; delete it by hand, then run the command again", storage.ErrConflict, filepath.Join(w.Common, "info", "attributes"))
		}
		user := data[end+len(attrEnd):]
		state, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
		if err != nil {
			return nil, err
		}
		install, _, err := loadInstallation(state)
		state.Close()
		if err != nil {
			return nil, err
		}
		if len(user) == 0 && !install.AttributesExisted {
			err = common.Remove("info/attributes")
		} else if exists {
			err = common.AtomicWrite("info/attributes", user, mode)
		}
		if err != nil {
			return nil, err
		}
	}
	includes, err := localIncludes(ctx, r, w.Root)
	if err != nil {
		return nil, err
	}
	set := classifyIncludes(includes, w.Common)
	for _, include := range append([]string{filterInclude}, set.legacy()...) {
		if err := unsetLocal(ctx, r, w.Root, "--fixed-value", "include.path", include); err != nil {
			return nil, err
		}
	}
	if err := unsetLocal(ctx, r, w.Root, requiredKey); err != nil {
		return nil, err
	}
	return found, nil
}
