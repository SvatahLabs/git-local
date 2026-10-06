package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
)

func completedInitPath(id string) string { return "transactions/completed/init-" + id + ".json" }

// AbandonInit discards the initialization journal of a checkout whose Git
// administrative directory no longer exists, so no checkout can resume it. It
// runs from any checkout of the repository. Metadata the journal had already
// published (its workspace registration and, for a new repository, the empty
// store skeleton) is removed only when it matches what the journal wrote. The
// journal is archived with outcome "abandoned". It returns the removed paths,
// relative to <common>/gitlocal.
func AbandonInit(ctx context.Context, r *gitexec.Runner, cwd, workspaceID string) ([]string, error) {
	removed := []string{}
	if !storage.ValidUUID(workspaceID) {
		return removed, errors.New("invalid workspace ID")
	}
	w, err := Discover(ctx, r, cwd)
	if err != nil {
		return removed, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return removed, err
	}
	defer common.Close()
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return removed, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return removed, err
	}
	defer d.Close()
	var j initJournal
	if err := d.ReadJSON("transactions/init.json", &j); errors.Is(err, os.ErrNotExist) {
		var done initJournal
		if e := d.ReadJSON(completedInitPath(workspaceID), &done); e != nil {
			return removed, e
		}
		if done.Outcome != "abandoned" {
			return removed, errors.New("initialization journal completed with a different outcome")
		}
		return removed, nil
	} else if err != nil {
		return removed, err
	}
	if err := validateJournalFields(j); err != nil {
		return removed, err
	}
	if j.Identity.WorkspaceID != workspaceID {
		return removed, fmt.Errorf("pending initialization is for workspace %s", j.Identity.WorkspaceID)
	}
	if _, err := os.Lstat(j.Workspace.Admin); err == nil {
		return removed, fmt.Errorf("%w: the checkout still exists; run git local init in %s", storage.ErrConflict, j.Workspace.Root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	// A checkout that moved keeps its published identity in its new
	// administrative directory; removing the registration would orphan it.
	if admin, err := PublishedIdentityAdmin(w.Common, workspaceID); err != nil {
		return removed, err
	} else if admin != "" {
		return removed, fmt.Errorf("%w: the checkout moved (its Git directory is now %s); run git local init in that checkout", storage.ErrConflict, admin)
	}
	base := filepath.Join("workspaces", workspaceID)
	if _, err := d.Lstat(base); err == nil {
		initialSelection := any(selection.Empty())
		if j.SelectionSchema == 0 {
			initialSelection = struct {
				SchemaVersion int    `json:"schemaVersion"`
				Generation    uint64 `json:"generation"`
				Include       []Rule `json:"include"`
			}{1, 0, []Rule{}}
		}
		entries, err := d.ReadDir(base)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			var want any
			switch entry.Name() {
			case "workspace.json":
				want = j.Workspace
			case "selection.json":
				want = initialSelection
			default:
				if !storage.IsTempName(entry.Name()) || !entry.Type().IsRegular() {
					return removed, fmt.Errorf("%w: unexpected entry %s in the abandoned registration", storage.ErrConflict, entry.Name())
				}
			}
			name := filepath.Join(base, entry.Name())
			if want != nil {
				if err := matchesJSON(d, name, want); err != nil {
					return removed, err
				}
			}
			if err := d.Remove(name); err != nil {
				return removed, err
			}
			removed = append(removed, filepath.ToSlash(name))
		}
		if err := d.RemoveDirectory(base); err != nil {
			return removed, err
		}
		removed = append(removed, filepath.ToSlash(base))
	} else if !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	if _, err := d.Lstat("repository.json"); errors.Is(err, os.ErrNotExist) && j.NewRepository {
		// Nothing was published: the store is the empty skeleton buildStore
		// writes, and an empty registry directory would block the next init.
		if _, err := d.Lstat("history.git"); err == nil {
			if err := emptySkeleton(w.Store); err != nil {
				return removed, err
			}
			if err := os.RemoveAll(w.Store); err != nil {
				return removed, err
			}
			removed = append(removed, "history.git")
		}
		if entries, err := d.ReadDir("workspaces"); err == nil && len(entries) == 0 {
			if err := d.RemoveDirectory("workspaces"); err != nil {
				return removed, err
			}
			removed = append(removed, "workspaces")
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	j.Outcome = "abandoned"
	if err := d.MkdirAll("transactions/completed"); err != nil {
		return removed, err
	}
	if err := d.WriteJSON(completedInitPath(workspaceID), j); err != nil {
		return removed, err
	}
	return removed, d.Remove("transactions/init.json")
}

// PublishedIdentityAdmin returns the Git administrative directory, the main
// one or a linked worktree's, whose identity file names workspaceID, or "" if
// none does. It reads the directories under the common directory, so it also
// finds a checkout that moved.
func PublishedIdentityAdmin(common, workspaceID string) (string, error) {
	admins := []string{common}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			admins = append(admins, filepath.Join(common, "worktrees", entry.Name()))
		}
	}
	for _, admin := range admins {
		dir, err := storage.OpenDir(admin)
		if err != nil {
			return "", err
		}
		var id Identity
		err = dir.ReadJSON("gitlocal-workspace.json", &id)
		dir.Close()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if id.WorkspaceID == workspaceID {
			return admin, nil
		}
	}
	return "", nil
}

func matchesJSON(d *storage.Dir, path string, v any) error {
	b, err := d.ReadFile(path, 1<<20)
	if err != nil {
		return err
	}
	want, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var a, c any
	if json.Unmarshal(b, &a) != nil || json.Unmarshal(want, &c) != nil || !reflect.DeepEqual(a, c) {
		return fmt.Errorf("%w: %s differs from the initialization journal", storage.ErrConflict, path)
	}
	return nil
}

// emptySkeleton accepts only the directories and files buildStore creates.
func emptySkeleton(path string) error {
	allowed := map[string]bool{".": true, "HEAD": true, "config": true, "objects": true, "objects/info": true, "objects/pack": true, "refs": true, "refs/heads": true, "refs/tags": true}
	return filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		if !allowed[filepath.ToSlash(rel)] && !storage.IsTempName(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: history store has content beyond the initialization skeleton (%s)", storage.ErrConflict, rel)
		}
		return nil
	})
}
