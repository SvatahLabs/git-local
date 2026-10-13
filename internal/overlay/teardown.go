package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	// Without the include or the required key no filter driver is defined,
	// and Git ignores an attribute naming an undefined filter; info/attributes
	// is read only when it may matter, so a repository that never used
	// overlays isn't held to what that read refuses (a symlink, over 1 MiB).
	if len(found) == 0 {
		return nil, nil
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
	state, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return nil, err
	}
	install, _, err := loadInstallation(state)
	state.Close()
	if err != nil {
		return nil, err
	}
	data, exists, mode, err := readOptional(common, "info/attributes", 1<<20)
	if err != nil {
		return nil, err
	}
	var unfiltered []string
	if begin, end := bytes.Index(data, []byte(attrBegin)), bytes.Index(data, []byte(attrEnd)); begin >= 0 || end >= 0 {
		edited := fmt.Errorf("%w: the git-local block in %s was edited; move your own lines out of it or delete it by hand, then run the command again", storage.ErrConflict, filepath.Join(w.Common, "info", "attributes"))
		if begin != 0 || end < 0 {
			return nil, edited
		}
		// Every line in the block must be one git-local writes, so a line a
		// user added inside it isn't dropped with it.
		if unfiltered, err = blockPaths(data[:end+len(attrEnd)]); err != nil {
			return nil, edited
		}
		user := data[end+len(attrEnd):]
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
	// The include and the required key go even when the installation found
	// them already set: the include names a file in the private state, which
	// deinit sets aside, and its final check refuses while either is left.
	for _, include := range append([]string{filterInclude}, set.legacy()...) {
		if err := unsetLocal(ctx, r, w.Root, "--fixed-value", "include.path", include); err != nil {
			return nil, err
		}
	}
	if err := unsetLocal(ctx, r, w.Root, requiredKey); err != nil {
		return nil, err
	}
	// Git may have cached a filtered file's size and time; see removeOverlay.
	for _, path := range unfiltered {
		if err := renewTimes(w.Root, path); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// blockPaths returns the paths of a managed attributes block, refusing a
// block with any line git-local doesn't write.
func blockPaths(block []byte) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(strings.TrimPrefix(string(block), attrBegin), attrEnd), "\n")
	var paths []string
	for _, line := range lines {
		if line == "" {
			continue
		}
		quoted, ok := strings.CutSuffix(line, " filter=gitlocal-overlay")
		if !ok {
			return nil, errors.New("unrecognized line in the managed attributes block")
		}
		literal, err := strconv.Unquote(quoted)
		if err != nil || !strings.HasPrefix(literal, "/") {
			return nil, errors.New("unrecognized line in the managed attributes block")
		}
		var path strings.Builder
		for i := 1; i < len(literal); i++ {
			if literal[i] == '\\' && i+1 < len(literal) {
				i++
			}
			path.WriteByte(literal[i])
		}
		paths = append(paths, path.String())
	}
	for _, render := range []func(string) string{attrPattern, legacyAttrPattern} {
		if want, err := renderBlock(paths, render); err == nil && bytes.Equal(want, block) {
			return paths, nil
		}
	}
	return nil, errors.New("unrecognized managed attributes block")
}

// CheckRemoveAll runs, without changing anything, the checks that removing
// the last of the registered overlays makes on the filter it then removes,
// so git local deinit refuses before removing any overlay rather than after
// removing all but the last. A missing installation record isn't refused:
// RemoveForDeinit removes the filter as installed by git-local.
func CheckRemoveAll(ctx context.Context, r *gitexec.Runner, root, commonPath string) error {
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return err
	}
	defer d.Close()
	if _, _, err := loadInstallation(d); err != nil {
		return err
	}
	includes, err := localIncludes(ctx, r, root)
	if err != nil {
		return err
	}
	set := classifyIncludes(includes, commonPath)
	if len(set.foreign) > 0 {
		return foreignIncludeError(set.foreign[0])
	}
	if set.relative+len(set.legacy()) > 1 {
		return errors.New("duplicate overlay filter include")
	}
	return nil
}
