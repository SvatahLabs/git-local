package configtx

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type HookConflict struct {
	Path, WorkspaceID, WorkspaceRoot string
}

type GuardSnapshot struct {
	Root, Common string
	Selections   []GuardSelection
}
type GuardSelection struct {
	WorkspaceID, WorkspaceRoot string
	State                      selection.State
}

// LoadGuardSnapshot reads only durable selection metadata. It deliberately
// avoids workspace discovery and private-history checks on the hook hot path.
func LoadGuardSnapshot(ctx context.Context, r *gitexec.Runner, cwd string) (GuardSnapshot, error) {
	var result GuardSnapshot
	for _, query := range []struct {
		target *string
		args   []string
	}{
		{&result.Root, []string{"rev-parse", "--show-toplevel"}},
		{&result.Common, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}},
	} {
		b, err := r.Guard(ctx, cwd, "", query.args...)
		if err != nil {
			return result, err
		}
		p := strings.TrimSpace(string(b))
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n") {
			return result, fmt.Errorf("invalid Git path %q", p)
		}
		*query.target = filepath.Clean(p)
	}
	common, err := storage.OpenDir(result.Common)
	if err != nil {
		return result, err
	}
	defer common.Close()
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return result, err
	}
	defer lock.Close()
	var repository workspace.Repository
	if err := common.ReadJSON("gitlocal/repository.json", &repository); err != nil {
		return result, err
	}
	if repository.SchemaVersion != 1 || !storage.ValidUUID(repository.RepositoryID) {
		return result, fmt.Errorf("invalid repository identity")
	}
	transactions, err := common.ReadDir("gitlocal/transactions")
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	for _, entry := range transactions {
		if entry.Name() != "completed" && !storage.IsTempName(entry.Name()) {
			return result, fmt.Errorf("unfinished configuration transaction %q", entry.Name())
		}
	}
	entries, err := common.ReadDir("gitlocal/workspaces")
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !storage.ValidUUID(id) {
			return result, fmt.Errorf("invalid workspace registration %q", id)
		}
		var record workspace.Record
		if err := common.ReadJSON(filepath.Join("gitlocal/workspaces", id, "workspace.json"), &record); err != nil {
			return result, err
		}
		if record.SchemaVersion != 1 || record.WorkspaceID != id || (record.State != "active" && record.State != "retained") {
			return result, fmt.Errorf("invalid workspace record %s", id)
		}
		var selected selection.State
		if err := common.ReadJSON(filepath.Join("gitlocal/workspaces", id, "selection.json"), &selected); err != nil {
			return result, err
		}
		selected, err = selection.Canonical(selected)
		if err != nil {
			return result, err
		}
		if len(selected.Include) != 0 {
			result.Selections = append(result.Selections, GuardSelection{id, record.Root, selected})
		}
	}
	return result, nil
}

func (s GuardSnapshot) Check(paths []string) ([]HookConflict, error) {
	conflicts := []HookConflict{}
	for _, p := range paths {
		if p == "" || strings.ContainsAny(p, "\x00\\") || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("invalid selected path %q", p)
		}
		for _, selected := range s.Selections {
			if selectedAlias(selected.State, p) {
				conflicts = append(conflicts, HookConflict{p, selected.WorkspaceID, selected.WorkspaceRoot})
				break
			}
		}
	}
	return conflicts, nil
}

func selectedAlias(state selection.State, staged string) bool {
	key := storage.AliasKey(staged)
	within := func(root string) bool { return key == root || strings.HasPrefix(key, root+"/") }
	for _, excluded := range state.Exclude {
		if within(storage.AliasKey(excluded)) {
			return false
		}
	}
	for _, rule := range state.Include {
		root := storage.AliasKey(rule.Path)
		if key == root || rule.Kind == "directory" && within(root) {
			return true
		}
	}
	return false
}

// CheckStagedPaths finds repository-relative paths selected by any active or
// retained workspace. It is also used to validate hook installation state.
func CheckStagedPaths(ctx context.Context, r *gitexec.Runner, cwd string, paths []string) ([]HookConflict, error) {
	s, err := openMode(ctx, r, cwd, true)
	if err != nil {
		return nil, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, pendingError(pending[0])
	}
	states := []struct {
		ID    string
		State selection.State
	}{}
	for _, src := range s.sources {
		record, err := src.record()
		if err != nil {
			return nil, err
		}
		if record.State != "active" && record.State != "retained" {
			continue
		}
		state, err := src.state()
		if err != nil {
			return nil, err
		}
		states = append(states, struct {
			ID    string
			State selection.State
		}{src.ID, state})
	}
	conflicts := []HookConflict{}
	for _, p := range paths {
		if p == "" || strings.ContainsAny(p, "\x00\\") || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("invalid staged path %q", p)
		}
		for _, state := range states {
			if selectedAlias(state.State, p) {
				conflicts = append(conflicts, HookConflict{Path: p, WorkspaceID: state.ID})
				break
			}
		}
	}
	return conflicts, nil
}
