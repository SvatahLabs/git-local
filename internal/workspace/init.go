package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
)

const SchemaVersion = 1

type Repository struct {
	SchemaVersion int    `json:"schemaVersion"`
	RepositoryID  string `json:"repositoryId"`
}
type Identity struct {
	SchemaVersion int    `json:"schemaVersion"`
	RepositoryID  string `json:"repositoryId"`
	WorkspaceID   string `json:"workspaceId"`
}
type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	WorkspaceID   string `json:"workspaceId"`
	CreatedAt     string `json:"createdAt"`
	Admin         string `json:"adminPath"`
	Root          string `json:"rootPath"`
	State         string `json:"state"`
}
type Rule = selection.Rule
type Selection = selection.State
type initJournal struct {
	SchemaVersion   int        `json:"schemaVersion"`
	Kind            string     `json:"kind"`
	Repository      Repository `json:"repository"`
	Identity        Identity   `json:"identity"`
	Workspace       Record     `json:"workspace"`
	NewRepository   bool       `json:"newRepository"`
	SelectionSchema int        `json:"selectionSchema,omitempty"`
	// Outcome is set only on a journal archived by AbandonInit.
	Outcome string `json:"outcome,omitempty"`
}

// Init is idempotent, and resumes only its own validated initialization journal.
func Init(ctx context.Context, r *gitexec.Runner, cwd string) (*Context, error) {
	return initialize(ctx, r, cwd, nil)
}
func initialize(ctx context.Context, r *gitexec.Runner, cwd string, fault func(string) error) (*Context, error) {
	if err := r.CheckCapabilities(ctx); err != nil {
		return nil, err
	}
	w, err := Discover(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	// An identity left by an interrupted git local deinit names a state that
	// no longer exists. Refuse before creating anything: a private state
	// directory left behind would block deinit and init in every worktree.
	if _, err := common.Lstat("gitlocal"); os.IsNotExist(err) {
		if err := staleIdentity(w); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err = common.MkdirAll("gitlocal/locks"); err != nil {
		return nil, err
	}
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	admin, err := storage.OpenDir(w.Admin)
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	var repo Repository
	repoErr := d.ReadJSON("repository.json", &repo)
	if repoErr != nil && !os.IsNotExist(repoErr) {
		return nil, repoErr
	}
	if repoErr == nil {
		if repo.SchemaVersion != SchemaVersion || !storage.ValidUUID(repo.RepositoryID) {
			return nil, errors.New("unsupported or malformed repository schema")
		}
	}
	var id Identity
	idErr := admin.ReadJSON("gitlocal-workspace.json", &id)
	if idErr != nil && !os.IsNotExist(idErr) {
		return nil, idErr
	}
	if idErr == nil && (id.SchemaVersion != SchemaVersion || !storage.ValidUUID(id.WorkspaceID) || !storage.ValidUUID(id.RepositoryID)) {
		return nil, errors.New("unsupported or malformed workspace identity")
	}
	var journal initJournal
	journalErr := d.ReadJSON("transactions/init.json", &journal)
	if journalErr != nil && !os.IsNotExist(journalErr) {
		return nil, journalErr
	}
	// Other unfinished transactions block init rather than being silently ignored.
	if entries, e := os.ReadDir(filepath.Join(w.Common, "gitlocal", "transactions")); e == nil {
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || storage.IsTempName(entry.Name()) && !entry.Type().IsRegular() {
				return nil, fmt.Errorf("%w: unsafe transaction entry %s", storage.ErrConflict, entry.Name())
			}
			// seed-files holds the file lists of large seed journals.
			if (entry.Name() == "completed" || entry.Name() == "seed-files") && entry.IsDir() {
				continue
			}
			if entry.Name() != "init.json" && !storage.IsTempName(entry.Name()) {
				if strings.HasPrefix(entry.Name(), "encrypt-") && strings.HasSuffix(entry.Name(), ".json") {
					id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "encrypt-"), ".json")
					if storage.ValidUUID(id) {
						return nil, fmt.Errorf("%w: unfinished encryption migration %s; run git local encrypt recover %s --resume or --rollback, or if both fail, git local recover %s --abandon", storage.ErrConflict, id, id, id)
					}
				}
				if strings.HasPrefix(entry.Name(), "seed-") && strings.HasSuffix(entry.Name(), ".json") {
					id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "seed-"), ".json")
					if storage.ValidUUID(id) {
						return nil, fmt.Errorf("%w: unfinished seed transaction %s; run git local recover %s --resume or --rollback, or if both fail, git local recover %s --abandon", storage.ErrConflict, id, id, id)
					}
				}
				return nil, fmt.Errorf("%w: unfinished or unknown transaction %s", storage.ErrConflict, entry.Name())
			}
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if err = validateTop(w.Common); err != nil {
		return nil, err
	}
	if journalErr == nil {
		// The identity is published last, so a checkout that holds it and has
		// moved since only needs the remaining steps, which find everything
		// already published.
		if idErr == nil && reflect.DeepEqual(id, journal.Identity) {
			err = validateJournalFields(journal)
		} else {
			err = validateJournal(journal, w)
		}
		if err != nil {
			return nil, err
		}
		if repoErr == nil && !reflect.DeepEqual(repo, journal.Repository) {
			return nil, fmt.Errorf("%w: repository differs from initialization journal", storage.ErrConflict)
		}
		if idErr == nil && !reflect.DeepEqual(id, journal.Identity) {
			return nil, fmt.Errorf("%w: identity differs from initialization journal", storage.ErrConflict)
		}
	} else {
		if idErr == nil {
			if repoErr != nil || id.RepositoryID != repo.RepositoryID {
				return nil, staleIdentityError(w, repoErr == nil)
			}
			w.RepositoryID = id.RepositoryID
			w.WorkspaceID = id.WorkspaceID
			if err = validateWorkspace(d, w); err != nil {
				return nil, err
			}
			if err = gitexec.ValidateStore(w.Store); err != nil {
				return nil, err
			}
			if err = checkDuplicates(ctx, r, w); err != nil {
				return nil, err
			}
			return w, nil
		}
		if repoErr != nil {
			// An absent repository marker without a journal is not permission to adopt
			// history or workspace files left by another tool.
			for _, name := range []string{"history.git", "workspaces"} {
				if _, e := d.Lstat(name); e == nil {
					return nil, fmt.Errorf("%w: unrecognized partial initialization at %s", storage.ErrConflict, name)
				} else if !os.IsNotExist(e) {
					return nil, e
				}
			}
			repo.RepositoryID, err = storage.UUID()
			if err != nil {
				return nil, err
			}
			repo.SchemaVersion = SchemaVersion
		} else if err = gitexec.ValidateStore(w.Store); err != nil {
			return nil, err
		}
		uuid, e := storage.UUID()
		if e != nil {
			return nil, e
		}
		journal = initJournal{SchemaVersion: SchemaVersion, Kind: "init", Repository: repo, Identity: Identity{SchemaVersion, repo.RepositoryID, uuid}, Workspace: Record{SchemaVersion, uuid, time.Now().UTC().Format(time.RFC3339Nano), w.Admin, w.Root, "active"}, NewRepository: repoErr != nil, SelectionSchema: selection.SchemaVersion}
		// Validate every other checkout before the journal exists: a failure
		// after it would leave a journal that blocks every command.
		w.RepositoryID, w.WorkspaceID = repo.RepositoryID, uuid
		if err = checkDuplicates(ctx, r, w); err != nil {
			return nil, err
		}
		if err = d.MkdirAll("transactions"); err != nil {
			return nil, err
		}
		if err = d.WriteJSON("transactions/init.json", journal); err != nil {
			return nil, err
		}
		if err = checkpoint(ctx, fault, "journal"); err != nil {
			return nil, err
		}
	}
	w.RepositoryID = journal.Identity.RepositoryID
	w.WorkspaceID = journal.Identity.WorkspaceID
	if err = checkDuplicates(ctx, r, w); err != nil {
		return nil, err
	}
	if journal.NewRepository {
		if err = buildStore(d, w.Store, ctx, fault); err != nil {
			return nil, err
		}
	} else if err = gitexec.ValidateStore(w.Store); err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, fault, "store"); err != nil {
		return nil, err
	}
	base := filepath.Join("workspaces", w.WorkspaceID)
	if err = d.MkdirAll(base); err != nil {
		return nil, err
	}
	if err = publishJSON(d, filepath.Join(base, "workspace.json"), journal.Workspace); err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, fault, "workspace"); err != nil {
		return nil, err
	}
	initialSelection := any(selection.Empty())
	if journal.SelectionSchema == 0 {
		initialSelection = struct {
			SchemaVersion int    `json:"schemaVersion"`
			Generation    uint64 `json:"generation"`
			Include       []Rule `json:"include"`
		}{1, 0, []Rule{}}
	}
	if err = publishJSON(d, filepath.Join(base, "selection.json"), initialSelection); err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, fault, "selection"); err != nil {
		return nil, err
	}
	if err = publishJSON(d, "repository.json", journal.Repository); err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, fault, "repository"); err != nil {
		return nil, err
	}
	if err = publishJSON(admin, "gitlocal-workspace.json", journal.Identity); err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, fault, "identity"); err != nil {
		return nil, err
	}
	if err = d.Remove("transactions/init.json"); err != nil {
		return nil, err
	}
	return w, nil
}
func checkpoint(ctx context.Context, f func(string) error, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f != nil {
		return f(name)
	}
	return nil
}
func publishJSON(d *storage.Dir, path string, v any) error {
	b, err := d.ReadFile(path, 1<<20)
	if os.IsNotExist(err) {
		return d.WriteJSON(path, v)
	}
	if err != nil {
		return err
	}
	want, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var a, c any
	if json.Unmarshal(b, &a) != nil || json.Unmarshal(want, &c) != nil || !reflect.DeepEqual(a, c) {
		return fmt.Errorf("%w: unexpected existing metadata %s", storage.ErrConflict, path)
	}
	return nil
}
func validateJournal(j initJournal, w *Context) error {
	if err := validateJournalFields(j); err != nil {
		return err
	}
	if j.Workspace.Admin != w.Admin || j.Workspace.Root != w.Root {
		return fmt.Errorf("%w: resume initialization from %s; moved or ambiguous mappings require explicit repair", storage.ErrConflict, j.Workspace.Root)
	}
	return nil
}
func validateJournalFields(j initJournal) error {
	if j.Outcome != "" || (j.SelectionSchema != 0 && j.SelectionSchema != selection.SchemaVersion) || j.SchemaVersion != SchemaVersion || j.Kind != "init" || j.Repository.SchemaVersion != SchemaVersion || j.Identity.SchemaVersion != SchemaVersion || j.Workspace.SchemaVersion != SchemaVersion || !storage.ValidUUID(j.Repository.RepositoryID) || !storage.ValidUUID(j.Identity.WorkspaceID) || j.Identity.RepositoryID != j.Repository.RepositoryID || j.Workspace.WorkspaceID != j.Identity.WorkspaceID || j.Workspace.State != "active" {
		return errors.New("invalid initialization journal; refusing automatic repair")
	}
	if _, err := time.Parse(time.RFC3339Nano, j.Workspace.CreatedAt); err != nil {
		return errors.New("invalid initialization timestamp")
	}
	if !filepath.IsAbs(j.Workspace.Admin) || !filepath.IsAbs(j.Workspace.Root) {
		return errors.New("invalid initialization paths")
	}
	return nil
}
func validateTop(common string) error {
	allowed := map[string]bool{"repository.json": true, "history.git": true, "locks": true, "transactions": true, "workspaces": true, "hook-install.json": true, "overlays": true, "overlay-filter.gitconfig": true, "overlay-install.json": true, "encryption.json": true, "quarantine": true}
	entries, err := os.ReadDir(filepath.Join(common, "gitlocal"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if (!allowed[entry.Name()] && !storage.IsTempName(entry.Name())) || entry.Type()&os.ModeSymlink != 0 || storage.IsTempName(entry.Name()) && !entry.Type().IsRegular() {
			return fmt.Errorf("unknown or symlinked extension state %s", entry.Name())
		}
	}
	return nil
}

// An empty Git format-v0 bare repository needs only these directories, HEAD and
// config. Creating the known skeleton ourselves makes every bootstrap write
// journal-replayable and avoids rerunning git init over unknown partial contents.
func buildStore(d *storage.Dir, path string, ctx context.Context, fault func(string) error) error {
	allowed := map[string]bool{".": true, "HEAD": true, "config": true, "objects": true, "objects/info": true, "objects/pack": true, "refs": true, "refs/heads": true, "refs/tags": true}
	if _, err := os.Lstat(path); err == nil {
		if err = filepath.WalkDir(path, func(p string, entry os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(path, p)
			if e != nil {
				return e
			}
			if (!allowed[filepath.ToSlash(rel)] && !storage.IsTempName(entry.Name())) || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("unknown partial history state %s", rel)
			}
			return nil
		}); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, dir := range []string{"history.git/objects/info", "history.git/objects/pack", "history.git/refs/heads", "history.git/refs/tags"} {
		if err := d.MkdirAll(dir); err != nil {
			return err
		}
		if err := checkpoint(ctx, fault, dir); err != nil {
			return err
		}
	}
	for _, entry := range []struct{ name, contents string }{{"history.git/HEAD", "ref: refs/heads/main\n"}, {"history.git/config", gitexec.StoreConfig}} {
		b, err := d.ReadFile(entry.name, 64<<10)
		if os.IsNotExist(err) {
			err = d.AtomicWrite(entry.name, []byte(entry.contents), 0600)
		} else if err == nil && string(b) != entry.contents {
			err = fmt.Errorf("%w: unexpected partial store file %s", storage.ErrConflict, entry.name)
		}
		if err != nil {
			return err
		}
		if err = checkpoint(ctx, fault, entry.name); err != nil {
			return err
		}
	}
	return gitexec.ValidateStore(path)
}
func validateWorkspace(d *storage.Dir, w *Context) error {
	var record Record
	if err := d.ReadJSON(filepath.Join("workspaces", w.WorkspaceID, "workspace.json"), &record); err != nil {
		return err
	}
	if record.SchemaVersion != SchemaVersion || record.WorkspaceID != w.WorkspaceID || record.State != "active" {
		return errors.New("invalid workspace record")
	}
	var selected Selection
	if err := d.ReadJSON(filepath.Join("workspaces", w.WorkspaceID, "selection.json"), &selected); err != nil {
		return err
	}
	_, err := selection.Canonical(selected)
	return err
}

// registeredAdmin finds the administrative directory Git registered for a
// linked worktree root from the gitdir pointers in the common directory,
// without the checkout's .git file that Git's own lookup needs.
func registeredAdmin(common, root string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue // Git keeps only directories here and ignores stray files (.DS_Store).
		}
		admin := filepath.Join(common, "worktrees", entry.Name())
		b, err := os.ReadFile(filepath.Join(admin, "gitdir"))
		if err != nil {
			continue
		}
		pointer := strings.TrimRight(string(b), "\r\n")
		if !filepath.IsAbs(pointer) {
			pointer = filepath.Join(admin, filepath.FromSlash(pointer))
		}
		dir := filepath.Dir(filepath.Clean(pointer))
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		if dir == root {
			return admin, nil
		}
	}
	return "", nil
}

// unreadableRegistrations refuses a linked worktree registration that holds
// a git-local identity but whose gitdir file can't be read: Git then leaves
// the worktree out of its list, so its workspace would go unchecked.
func unreadableRegistrations(common string) error {
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue // Git keeps only directories here and ignores stray files (.DS_Store).
		}
		admin := filepath.Join(common, "worktrees", entry.Name())
		gitdir := filepath.Join(admin, "gitdir")
		file, err := os.Open(gitdir)
		if err == nil {
			file.Close()
			continue
		}
		if os.IsNotExist(err) {
			continue
		}
		if _, e := os.Lstat(filepath.Join(admin, "gitlocal-workspace.json")); e == nil {
			return fmt.Errorf("%w: worktree registration %s holds a git-local workspace, but its gitdir file can't be read (%v), so Git doesn't list the worktree; fix the file's permissions", storage.ErrConflict, gitdir, err)
		} else if !os.IsNotExist(e) {
			return fmt.Errorf("worktree registration %s: gitdir file can't be read (%v), and whether it holds a git-local workspace can't be checked: %w", admin, err, e)
		}
	}
	return nil
}

func checkDuplicates(ctx context.Context, r *gitexec.Runner, w *Context) error {
	roots, err := registeredRoots(ctx, r, w)
	if err != nil {
		return err
	}
	if err := unreadableRegistrations(w.Common); err != nil {
		return err
	}
	seen := map[string]string{w.WorkspaceID: w.Admin}
	w.Warnings = nil
	for _, root := range roots {
		if _, e := StatRoot(root); e != nil {
			if root == w.Root {
				return e
			}
			if !os.IsNotExist(e) {
				w.Warnings = append(w.Warnings, fmt.Sprintf("registered worktree %s is unavailable (%v); its identity was not checked", root, e))
			}
			continue
		}
		// Never propagate the caller's GIT_DIR into discovery of another checkout.
		var e error
		var adminPath string
		if root == w.Root {
			adminPath = w.Admin
		} else {
			// Registered linked-worktree admin directories are obtained from Git's own
			// gitfile resolver, with repository-selection environment removed.
			adminPath, e = r.AdminAt(ctx, root)
			if e != nil {
				// The directory exists but isn't a checkout any more, for
				// example because its .git file was deleted.
				registered, re := registeredAdmin(w.Common, root)
				if re != nil {
					return re
				}
				if registered != "" {
					if _, re = os.Lstat(filepath.Join(registered, "gitlocal-workspace.json")); re == nil {
						return fmt.Errorf("%w: registered worktree %s holds a git-local workspace but is not a Git checkout (%v); run git worktree repair %s to restore its .git file", storage.ErrConflict, root, e, root)
					} else if !os.IsNotExist(re) {
						return re
					}
				}
				w.Warnings = append(w.Warnings, fmt.Sprintf("registered worktree %s is not a Git checkout (%v); skipped it. Run git worktree repair %s if it should be one, or git worktree remove --force %s", root, e, root, root))
				continue
			}
		}
		dir, e := storage.OpenDir(adminPath)
		if e != nil {
			return e
		}
		var id Identity
		e = dir.ReadJSON("gitlocal-workspace.json", &id)
		dir.Close()
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if id.SchemaVersion != SchemaVersion || id.RepositoryID != w.RepositoryID || !storage.ValidUUID(id.WorkspaceID) {
			return fmt.Errorf("%w: inconsistent identity at %s", storage.ErrConflict, adminPath)
		}
		if prev, ok := seen[id.WorkspaceID]; ok && prev != adminPath {
			return fmt.Errorf("%w: duplicate workspace identity in %s and %s", storage.ErrConflict, prev, adminPath)
		}
		seen[id.WorkspaceID] = adminPath
	}
	return nil
}

// staleIdentity refuses when the worktree w holds an identity file while no
// private state exists.
func staleIdentity(w *Context) error {
	if _, err := os.Lstat(filepath.Join(w.Admin, "gitlocal-workspace.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return staleIdentityError(w, false)
}

// staleIdentityError names the way out for an identity file of a private
// state that no longer exists. Without any private state, git local deinit
// removes it; with a live one (built since by another worktree's init),
// deinit would remove that, so the file is to be deleted by hand: nothing
// refers to it.
func staleIdentityError(w *Context, live bool) error {
	name := filepath.Join(w.Admin, "gitlocal-workspace.json")
	if live {
		return fmt.Errorf("%w: this worktree's git local identity (%s) belongs to an earlier private state, which an interrupted git local deinit leaves; nothing refers to it: delete it, then run git local init again", storage.ErrConflict, name)
	}
	return fmt.Errorf("%w: this worktree's git local identity (%s) belongs to a private state that no longer exists, which an interrupted git local deinit leaves; run git local deinit here to remove it, then git local init", storage.ErrConflict, name)
}
