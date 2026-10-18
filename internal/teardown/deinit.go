// Package teardown removes git-local from a repository (git local deinit).
package teardown

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/hook"
	"gitlocal/internal/overlay"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// Result reports what deinit removed. Archive is the kept private state,
// relative to the common Git directory; it's empty when nothing was kept.
type Result struct {
	Overlays   []string `json:"overlays"`
	Hooks      []string `json:"hooks"`
	Exclude    bool     `json:"excludeBlockRemoved"`
	Identities []string `json:"identities"`
	Archive    string   `json:"archive,omitempty"`
	Removed    bool     `json:"historyRemoved"`
	Warnings   []string `json:"warnings"`
}

// Deinit removes git-local from the repository at cwd and every worktree:
// overlays, hooks, the managed info/exclude block, the private state and each
// worktree's identity file. It refuses while a transaction is pending or an
// overlay's working file holds changes that aren't saved in the overlay.
//
// Each step can be run again, so an interrupted deinit is finished by running
// it again. Every check that can refuse runs before the first change. The
// private state directory is renamed (kept) in one step, after everything that
// needs it, under the exclusive repository lock and after a final check that
// no other git local command installed anything meanwhile; removeHistory
// deletes it instead, only when no quarantine or restore recovery ref would be
// lost.
func Deinit(ctx context.Context, r *gitexec.Runner, cwd string, removeHistory bool) (Result, error) {
	result := Result{Overlays: []string{}, Hooks: []string{}, Identities: []string{}, Warnings: []string{}}
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return result, err
	}
	defer common.Close()
	// belongs reports whether an identity file belongs to the state this
	// deinit removes. Another worktree's git local init may build new state
	// once the old one is set aside, and keeps its identity.
	_, err = common.Lstat("gitlocal")
	if err == nil {
		// A git local init refused after an interrupted deinit can leave a
		// private state directory holding only its lock: no live state.
		var bare bool
		if bare, err = removeBareState(ctx, common); err == nil && bare {
			err = os.ErrNotExist
		}
	}
	if err == nil {
		if err := checkInitialized(w, common); err != nil {
			return result, err
		}
		if err := checkIdentity(w, common); err != nil {
			return result, err
		}
		if err := checkRemovable(ctx, r, w, removeHistory); err != nil {
			return result, err
		}
		if err := configtx.CheckExcludeBlock(ctx, r, cwd); err != nil {
			return result, err
		}
		var policy secureblob.Policy
		if err := common.ReadJSON(secureblob.PolicyFile, &policy); err != nil && !os.IsNotExist(err) {
			return result, err
		}
		// The hook records are checked before the first change too: a
		// refusal after overlays were removed would leave deinit half done.
		if err := hook.CheckUninstall(ctx, r, cwd); err != nil {
			return result, err
		}
		var warnings []string
		result.Overlays, warnings, err = removeOverlays(ctx, r, w)
		result.Warnings = append(result.Warnings, warnings...)
		if err != nil {
			return result, err
		}
		// An overlay add interrupted before its manifest, whose directory was
		// then deleted, leaves the filter with no overlay to remove.
		leftovers, warnings, err := overlay.RemoveFilter(ctx, r, cwd)
		result.Warnings = append(result.Warnings, warnings...)
		if err != nil {
			return result, err
		}
		if len(leftovers) > 0 && len(result.Overlays) == 0 {
			result.Warnings = append(result.Warnings, "removed the overlay filter an interrupted git local overlay add left ("+strings.Join(leftovers, ", ")+")")
		}
		for _, uninstall := range []func() ([]string, []string, error){
			func() ([]string, []string, error) { return hook.UninstallSnapshots(ctx, r, cwd) },
			func() ([]string, []string, error) { return hook.Uninstall(ctx, r, cwd, false, false) },
		} {
			paths, warnings, err := uninstall()
			result.Hooks = append(result.Hooks, paths...)
			result.Warnings = append(result.Warnings, warnings...)
			if err != nil {
				return result, err
			}
		}
		if result.Exclude, err = configtx.RemoveExcludeBlock(ctx, r, cwd); err != nil {
			return result, err
		}
		if result.Exclude && len(policy.Sensitive) > 0 {
			var paths []string
			for _, rule := range policy.Sensitive {
				paths = append(paths, rule.Path)
			}
			result.Warnings = append(result.Warnings, "sensitive paths "+strings.Join(paths, ", ")+" are no longer ignored or guarded: git add -A would stage them and git clean -fd would delete them; add them to .git/info/exclude if Git should keep ignoring them")
		}
		if result.Archive, result.Removed, err = setAside(ctx, r, w, common, removeHistory); err != nil {
			return result, err
		}
	} else if !os.IsNotExist(err) {
		return result, err
	} else {
		// A rerun after the state was set aside (or a deinit interrupted
		// while deleting it) names what an earlier run kept.
		kept, err := filepath.Glob(filepath.Join(w.Common, "gitlocal-removed-*"))
		if err != nil {
			return result, err
		}
		for _, dir := range kept {
			result.Warnings = append(result.Warnings, "the private state an earlier git local deinit set aside is in "+dir+"; delete it when you no longer need it")
		}
	}
	beforeRemoveIdentities()
	// Identities that don't belong to the live state, which another
	// worktree's git local init may have built meanwhile, are removed: the
	// removed state's, and any of an older one an interrupted deinit left.
	// When a private state directory exists, the shared lock keeps an init
	// from publishing a new state's identity meanwhile. When none exists (the
	// usual case once the state is set aside) no lock is taken: an init in a
	// worktree without an identity that creates a state after the check
	// below, and publishes its identity before removeIdentities reaches that
	// worktree, loses the identity file. init refuses while another worktree
	// still holds an identity, which narrows that window but doesn't close it.
	release, belongs, err := liveIdentities(ctx, common)
	if err != nil {
		return result, err
	}
	defer release()
	result.Identities, err = removeIdentities(common, w.Common, func(id workspace.Identity) bool { return !belongs(id) })
	return result, err
}

// liveIdentities reports which identity files belong to the live private
// state, holding its lock until release; with no live state, none does.
func liveIdentities(ctx context.Context, common *storage.Dir) (func(), func(workspace.Identity) bool, error) {
	none := func(workspace.Identity) bool { return false }
	if _, err := common.Lstat("gitlocal"); os.IsNotExist(err) {
		return func() {}, none, nil
	} else if err != nil {
		return nil, nil, err
	}
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return nil, nil, err
	}
	var live workspace.Repository
	if err := common.ReadJSON("gitlocal/repository.json", &live); os.IsNotExist(err) {
		return func() { lock.Close() }, none, nil
	} else if err != nil {
		lock.Close()
		return nil, nil, err
	}
	return func() { lock.Close() }, func(id workspace.Identity) bool { return id.RepositoryID == live.RepositoryID }, nil
}

// removeBareState removes a private state directory that holds nothing but
// its lock file (a git local init refused before writing anything leaves
// one), or nothing (a deinit interrupted between its last two removals leaves
// that), under the exclusive lock, and reports whether it was one. On POSIX
// systems a command waiting for the lock meanwhile finds it removed and
// stops. On Windows the lock is closed before the file is deleted, so a
// waiter may take it and go on; deinit then fails because the file is in use
// (or its directory isn't empty), and running it again finishes.
func removeBareState(ctx context.Context, common *storage.Dir) (bool, error) {
	bare := func() (bool, error) {
		entries, err := common.ReadDir("gitlocal")
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if entry.Name() != "locks" || !entry.IsDir() {
				return false, nil
			}
		}
		locks, err := common.ReadDir("gitlocal/locks")
		if os.IsNotExist(err) {
			return true, nil
		} else if err != nil {
			return false, err
		}
		for _, entry := range locks {
			if entry.Name() != "repository.lock" || !entry.Type().IsRegular() {
				return false, nil
			}
		}
		return true, nil
	}
	if ok, err := bare(); err != nil || !ok {
		return false, err
	}
	if err := common.MkdirAll("gitlocal/locks"); err != nil {
		return false, err
	}
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if ok, err := bare(); err != nil || !ok {
		return false, err
	}
	if runtime.GOOS == "windows" {
		// Windows can't delete a file that is open.
		lock.Close()
	}
	for _, name := range []string{"gitlocal/locks/repository.lock", "gitlocal/locks", "gitlocal"} {
		var err error
		if name == "gitlocal/locks/repository.lock" {
			err = common.Remove(name)
		} else {
			err = common.RemoveDirectory(name)
		}
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	return true, nil
}

// beforeRemoveIdentities runs just before deinit removes the identity files;
// tests replace it to run git local init meanwhile.
var beforeRemoveIdentities = func() {}

// beforeSetAside runs just before deinit takes the lock to set the private
// state aside; tests replace it to install something meanwhile.
var beforeSetAside = func() {}

// setAside renames the private state to gitlocal-removed-<id>, or with
// removeHistory deletes it, under the exclusive repository lock. Each earlier
// step took the lock on its own, so another git local command (overlay add,
// hook install, auto install, track) may have run since; the final check
// refuses then, leaving everything in place, and a rerun removes what it
// installed. On POSIX systems, commands that wait for the lock meanwhile find
// no private state afterwards and stop. On Windows the lock is closed before
// the rename, so a waiter may take it and go on; the rename then fails because
// a file in the directory is in use, and deinit can be run again.
func setAside(ctx context.Context, r *gitexec.Runner, w *workspace.Context, common *storage.Dir, removeHistory bool) (string, bool, error) {
	beforeSetAside()
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return "", false, err
	}
	defer lock.Close()
	if err := finalCheck(ctx, r, w, common, removeHistory); err != nil {
		return "", false, err
	}
	id, err := storage.UUID()
	if err != nil {
		return "", false, err
	}
	archive := "gitlocal-removed-" + id
	if runtime.GOOS == "windows" {
		// Windows can't rename a directory while a file in it (the lock) is
		// open; the check above still ran under the lock.
		lock.Close()
	}
	if err := common.RenameNoReplace("gitlocal", archive); err != nil {
		return "", false, err
	}
	if !removeHistory {
		return archive, false, nil
	}
	if err := os.RemoveAll(filepath.Join(w.Common, archive)); err != nil {
		return "", false, fmt.Errorf("delete the private state %s: %w", filepath.Join(w.Common, archive), err)
	}
	return "", true, nil
}

// finalCheck looks, without taking a lock, for anything git-local installed
// after deinit removed it: a pending transaction, an overlay, the overlay
// filter, a hook record or the managed info/exclude block. With removeHistory
// it also checks again, under the caller's lock, that the deletion loses no
// data kept nowhere else: a restore elsewhere may have finished since
// checkRemovable, leaving its recovery ref.
func finalCheck(ctx context.Context, r *gitexec.Runner, w *workspace.Context, common *storage.Dir, removeHistory bool) error {
	var found []string
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return err
	}
	defer meta.Close()
	if removeHistory {
		lost, err := historyLosses(ctx, r, w, meta)
		if err != nil {
			return err
		}
		if len(lost) > 0 {
			return removeHistoryRefusal(lost)
		}
	}
	pending, err := configtx.PendingLocked(meta)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		found = append(found, "transaction "+pending[0])
	}
	versions, err := overlay.RegisteredLocked(w.Common)
	if err != nil {
		return err
	}
	for _, v := range versions {
		found = append(found, "overlay "+v.Manifest.Name)
	}
	leftovers, err := overlay.FilterLeftovers(ctx, r, w.Root, w.Common)
	if err != nil {
		return err
	}
	found = append(found, leftovers...)
	if _, err := meta.Lstat("hook-install.json"); err == nil {
		found = append(found, "hooks")
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := common.ReadFile("info/exclude", 1<<20)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if bytes.Contains(data, []byte(exclude.Begin)) {
		found = append(found, "the managed block in info/exclude")
	}
	if len(found) > 0 {
		return fmt.Errorf("%w: another git local command installed %s while deinit ran; run git local deinit again", storage.ErrConflict, strings.Join(found, ", "))
	}
	return nil
}

// checkInitialized refuses in a worktree that git local init never ran in,
// where the steps that need a workspace would fail after earlier ones made
// changes, and names the worktrees to run deinit from instead.
func checkInitialized(w *workspace.Context, common *storage.Dir) error {
	if _, err := os.Stat(filepath.Join(w.Admin, "gitlocal-workspace.json")); err == nil || !os.IsNotExist(err) {
		return err
	}
	return fmt.Errorf("%w: git local was never initialized in this worktree; run git local deinit from %s", storage.ErrConflict, deinitRoots(w, common))
}

// deinitRoots names the other worktrees deinit can run from: those whose
// identity file can be read and belongs to the live state (checkIdentity
// refuses in the others).
func deinitRoots(w *workspace.Context, common *storage.Dir) string {
	var live workspace.Repository
	_ = common.ReadJSON("gitlocal/repository.json", &live)
	// usable reports whether the identity file named (relative to the
	// common directory) can be read and belongs to the live state.
	usable := func(name string) bool {
		var id workspace.Identity
		return common.ReadJSON(name, &id) == nil && (live.RepositoryID == "" || id.RepositoryID == live.RepositoryID)
	}
	var roots []string
	if filepath.Clean(w.Admin) != filepath.Clean(w.Common) && usable("gitlocal-workspace.json") {
		if root := mainRoot(w.Common); root != "" {
			roots = append(roots, root)
		}
	}
	// A linked worktree's gitdir file names its .git file, also after
	// git worktree move; Git may write it relative to the worktree's
	// administrative directory.
	linked, _ := filepath.Glob(filepath.Join(w.Common, "worktrees", "*", "gitlocal-workspace.json"))
	for _, identity := range linked {
		admin := filepath.Dir(identity)
		if filepath.Clean(admin) == filepath.Clean(w.Admin) || !usable(filepath.Join("worktrees", filepath.Base(admin), "gitlocal-workspace.json")) {
			continue
		}
		gitdir, err := os.ReadFile(filepath.Join(admin, "gitdir"))
		if err != nil {
			continue
		}
		dotgit := strings.TrimSpace(string(gitdir))
		if !filepath.IsAbs(dotgit) {
			dotgit = filepath.Join(admin, dotgit)
		}
		roots = append(roots, filepath.Dir(filepath.Clean(dotgit)))
	}
	sort.Strings(roots)
	if len(roots) == 0 {
		return "a worktree where git local init ran"
	}
	return strings.Join(roots, " or ")
}

// checkIdentity refuses when this worktree's identity belongs to an earlier
// private state than the live one, which another worktree's git local init
// built after an interrupted deinit: deinit from here would remove a state
// this worktree isn't part of. It also refuses, naming the way out, when the
// identity can't be read: the steps that open the workspace need it.
func checkIdentity(w *workspace.Context, common *storage.Dir) error {
	var id workspace.Identity
	var live workspace.Repository
	if err := common.ReadJSON("gitlocal/repository.json", &live); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	admin, err := storage.OpenDir(w.Admin)
	if err != nil {
		return err
	}
	defer admin.Close()
	name := filepath.Join(w.Admin, "gitlocal-workspace.json")
	if err := admin.ReadJSON("gitlocal-workspace.json", &id); os.IsNotExist(err) {
		return err
	} else if err != nil {
		way := "run git local deinit from " + deinitRoots(w, common) + ", which removes it too"
		if workspaceID := recordedWorkspace(common, w.Admin); workspaceID != "" {
			content, _ := json.Marshal(workspace.Identity{SchemaVersion: workspace.SchemaVersion, RepositoryID: live.RepositoryID, WorkspaceID: workspaceID})
			way = "replace its content with " + string(content) + " (the workspace the private state records for this worktree) and run git local deinit again, or " + way
		}
		return fmt.Errorf("%w: this worktree's git local identity (%s) can't be read (%v), and deinit needs it here; %s", storage.ErrConflict, name, err, way)
	}
	if id.RepositoryID != live.RepositoryID {
		return fmt.Errorf("%w: this worktree's git local identity (%s) belongs to an earlier private state, which an interrupted git local deinit leaves, not to the one in %s; delete that file, then run git local deinit from a worktree where git local init ran since", storage.ErrConflict, name, filepath.Join(w.Common, "gitlocal"))
	}
	return nil
}

// recordedWorkspace is the ID of the active workspace the private state
// records for the worktree whose administrative directory is admin, or "".
func recordedWorkspace(common *storage.Dir, admin string) string {
	entries, _ := common.ReadDir("gitlocal/workspaces")
	for _, entry := range entries {
		var record workspace.Record
		if !entry.IsDir() || !storage.ValidUUID(entry.Name()) || common.ReadJSON(filepath.Join("gitlocal", "workspaces", entry.Name(), "workspace.json"), &record) != nil {
			continue
		}
		if record.State == "active" && filepath.Clean(record.Admin) == filepath.Clean(admin) {
			return entry.Name()
		}
	}
	return ""
}

// mainRoot is the main worktree of the repository whose common directory is
// common: the root its workspace record holds (a --separate-git-dir layout
// keeps the Git directory elsewhere), or else the parent of a .git directory.
func mainRoot(common string) string {
	if meta, err := storage.OpenDir(filepath.Join(common, "gitlocal")); err == nil {
		defer meta.Close()
		entries, _ := meta.ReadDir("workspaces")
		for _, entry := range entries {
			var record workspace.Record
			if !entry.IsDir() || !storage.ValidUUID(entry.Name()) || meta.ReadJSON(filepath.Join("workspaces", entry.Name(), "workspace.json"), &record) != nil {
				continue
			}
			if record.State == "active" && filepath.IsAbs(record.Root) && filepath.Clean(record.Admin) == filepath.Clean(common) {
				return record.Root
			}
		}
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return ""
}

// checkRemovable refuses what deinit can't undo safely: a pending
// transaction, and with removeHistory, a quarantine or recovery ref.
func checkRemovable(ctx context.Context, r *gitexec.Runner, w *workspace.Context, removeHistory bool) error {
	pending, err := configtx.PendingJournals(ctx, r, w.Root)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: transaction %s is unfinished; resolve it first (git local doctor names the command)", storage.ErrConflict, pending[0].ID)
	}
	if !removeHistory {
		return nil
	}
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return err
	}
	defer meta.Close()
	lost, err := historyLosses(ctx, r, w, meta)
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		return removeHistoryRefusal(lost)
	}
	return nil
}

// historyLosses lists what deleting the private state would lose that exists
// nowhere else: quarantines, restore recovery refs (the previous file versions
// of earlier restores) and retired workspaces' history (the only copy of their
// private files). It reads the records directly, so it can run under the
// repository lock.
func historyLosses(ctx context.Context, r *gitexec.Runner, w *workspace.Context, meta *storage.Dir) ([]string, error) {
	var lost []string
	quarantine, err := meta.ReadDir("quarantine")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range quarantine {
		lost = append(lost, filepath.Join(w.Common, "gitlocal", "quarantine", entry.Name()))
	}
	refs, err := r.HistoryListing(ctx, w.Store, nil, "show-ref")
	var ge *gitexec.Error
	if err != nil && !(errors.As(err, &ge) && ge.Code == 1) {
		return nil, err
	}
	for _, line := range strings.Split(string(bytes.TrimSpace(refs)), "\n") {
		if _, ref, ok := strings.Cut(line, " "); ok && strings.HasPrefix(ref, "refs/gitlocal/recovery/") {
			lost = append(lost, ref)
		}
	}
	workspaces, err := meta.ReadDir("workspaces")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range workspaces {
		if !entry.IsDir() || !storage.ValidUUID(entry.Name()) {
			continue
		}
		var record workspace.Record
		if err := meta.ReadJSON(filepath.Join("workspaces", entry.Name(), "workspace.json"), &record); err != nil {
			return nil, err
		}
		if record.State == "retained" {
			lost = append(lost, "the history of retired workspace "+entry.Name())
		}
	}
	return lost, nil
}

func removeHistoryRefusal(lost []string) error {
	return fmt.Errorf("%w: --remove-history would delete %s, which hold data kept nowhere else (previous file versions of earlier restores, quarantined data, or a retired worktree's files). Run git local deinit without --remove-history: it keeps the private state in the common Git directory as gitlocal-removed-<id>, which you can delete once you no longer need it", storage.ErrConflict, strings.Join(lost, ", "))
}

// removeOverlays deactivates every overlay. Each must be removed from its own
// worktree, or from here when its workspace is retained.
func removeOverlays(ctx context.Context, r *gitexec.Runner, w *workspace.Context) ([]string, []string, error) {
	versions, err := overlay.Registered(ctx, w.Common)
	if err != nil || len(versions) == 0 {
		return []string{}, nil, err
	}
	workspaces, err := configtx.ListWorkspaces(ctx, r, w.Root)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]configtx.WorkspaceInfo{}
	for _, info := range workspaces {
		byID[info.ID] = info
	}
	var problems []string
	for _, v := range versions {
		info := byID[v.Manifest.WorkspaceID]
		switch {
		case info.State == "retained":
		case info.Available:
			if v.Manifest.State == "removing" {
				continue
			}
			working, err := os.ReadFile(filepath.Join(info.Root, filepath.FromSlash(v.Manifest.Path)))
			if err != nil && !os.IsNotExist(err) {
				return nil, nil, err
			}
			if err != nil {
				continue
			}
			// The filter writes the saved patch applied to the file's
			// current index version, which upstream may have changed since
			// the overlay was saved.
			expected, err := workingImage(ctx, r, info.Root, v)
			if errors.Is(err, overlay.ErrConflict) {
				problems = append(problems, fmt.Sprintf("the overlay %s no longer applies to %s in %s; resolve it first (git local overlay status)", v.Manifest.Name, v.Manifest.Path, info.Root))
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if !bytes.Equal(working, expected) {
				problems = append(problems, fmt.Sprintf("%s in %s has changes the overlay %s doesn't hold; save them with git local overlay add %s -- %s, or undo them", v.Manifest.Path, info.Root, v.Manifest.Name, v.Manifest.Name, v.Manifest.Path))
			}
		default:
			problems = append(problems, fmt.Sprintf("overlay %s belongs to workspace %s, whose worktree is %s; reconnect it, or prune it with git local workspace prune", v.Manifest.Name, v.Manifest.WorkspaceID, info.State))
		}
	}
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("%w: %s", storage.ErrConflict, strings.Join(problems, "; "))
	}
	// Removing the last overlay also removes the filter; its checks run
	// before the first overlay is removed.
	if err := overlay.CheckRemoveAll(ctx, r, w.Root, w.Common); err != nil {
		return nil, nil, err
	}
	removed := []string{}
	var warnings []string
	for _, v := range versions {
		info := byID[v.Manifest.WorkspaceID]
		var more []string
		var err error
		if info.State == "retained" {
			_, more, err = overlay.RemoveForDeinit(ctx, r, w.Root, v.Manifest.WorkspaceID, v.Manifest.Name)
		} else {
			_, more, err = overlay.RemoveForDeinit(ctx, r, info.Root, "", v.Manifest.Name)
		}
		warnings = append(warnings, more...)
		if err != nil {
			return removed, warnings, fmt.Errorf("remove overlay %s: %w", v.Manifest.Name, err)
		}
		removed = append(removed, v.Manifest.Name)
	}
	return removed, warnings, nil
}

// workingImage is the working file the overlay filter writes for v: its
// saved patch applied to the file's version in the worktree's index.
func workingImage(ctx context.Context, r *gitexec.Runner, root string, v overlay.Version) ([]byte, error) {
	index, err := r.IndexBlob(ctx, root, "", v.Manifest.Path)
	if err != nil {
		return nil, err
	}
	return overlay.Apply(ctx, r.Executable(), v.Base, v.Local, index)
}

// removeIdentities deletes the workspace identity file of the main worktree
// (in the common directory) and of each linked worktree that belongs to the
// removed state; an unreadable one is deleted too.
func removeIdentities(common *storage.Dir, commonPath string, belongs func(workspace.Identity) bool) ([]string, error) {
	names := []string{"gitlocal-workspace.json"}
	linked, err := os.ReadDir(filepath.Join(commonPath, "worktrees"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range linked {
		if entry.IsDir() {
			names = append(names, filepath.Join("worktrees", entry.Name(), "gitlocal-workspace.json"))
		}
	}
	removed := []string{}
	for _, name := range names {
		var id workspace.Identity
		if err := common.ReadJSON(name, &id); err == nil && !belongs(id) {
			continue
		} else if os.IsNotExist(err) {
			continue
		}
		if err := common.Remove(name); err == nil {
			removed = append(removed, filepath.Join(commonPath, name))
		} else if !os.IsNotExist(err) {
			return removed, err
		}
	}
	return removed, nil
}
