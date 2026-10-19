package restore

import (
	"context"
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/secureblob"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// Plan admits a source snapshot and describes only changes to recorded, currently
// selected paths. The shared repository lock protects cooperating configuration
// writers; external Git and filesystem changes are checked again before return.
func Plan(ctx context.Context, r *gitexec.Runner, cwd, sourceWorkspace, selector string, paths []string, overwrite bool) (RestorePlan, error) {
	return plan(ctx, r, cwd, sourceWorkspace, selector, paths, overwrite, nil)
}
func plan(ctx context.Context, r *gitexec.Runner, cwd, sourceWorkspace, selector string, paths []string, overwrite bool, beforeRecheck func() error) (RestorePlan, error) {
	var result RestorePlan
	if err := history.ValidateSelector(selector); err != nil {
		return result, err
	}
	if len(paths) == 0 {
		return result, fmt.Errorf("restore requires explicit paths")
	}
	err := configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, state selection.State, warnings []string, recheck func() error) error {
		if len(warnings) > 0 {
			return fmt.Errorf("%w: repair configuration before restore planning: %s", storage.ErrConflict, strings.Join(warnings, "; "))
		}
		var err error
		result, err = planLocked(ctx, r, cwd, w, state, recheck, sourceWorkspace, selector, paths, overwrite, beforeRecheck)
		return err
	})
	return result, err
}
func planLocked(ctx context.Context, r *gitexec.Runner, cwd string, w workspace.Context, state selection.State, recheck func() error, sourceWorkspace, selector string, paths []string, overwrite bool, beforeRecheck func() error) (RestorePlan, error) {
	var result RestorePlan
	err := func() error {
		sourceHead, err := history.WorkspaceHeadLocked(ctx, r, w, sourceWorkspace)
		if err != nil {
			return err
		}
		source, err := history.ResolveLocked(ctx, r, w, sourceWorkspace, selector)
		if err != nil {
			return err
		}
		baselineID, err := history.WorkspaceHeadLocked(ctx, r, w, w.WorkspaceID)
		if err != nil {
			return err
		}
		baseline := history.Snapshot{Files: map[string]history.SnapshotFile{}}
		if baselineID != "" {
			baseline, err = history.ResolveLocked(ctx, r, w, w.WorkspaceID, baselineID)
			if err != nil {
				return err
			}
		}
		engine, err := selection.New(r, w.Root, w.Admin)
		if err != nil {
			return err
		}
		defer engine.Close()
		files, err := storage.OpenDir(w.Root)
		if err != nil {
			return err
		}
		defer files.Close()
		requests := []string{}
		for _, arg := range paths {
			p, e := engine.Normalize(cwd, arg)
			if e != nil {
				return e
			}
			selected := state.Selected(p)
			for _, rule := range state.Include {
				if within(p, rule.Path) {
					selected = true
				}
			}
			if !selected || state.Excluded(p) {
				return fmt.Errorf("%w: %q is not currently selected; use track or track --from first", storage.ErrConflict, p)
			}
			requests = append(requests, p)
		}
		requests = normalizeRequests(requests)
		candidates := map[string]bool{}
		for _, entries := range []map[string]history.SnapshotFile{source.Files, baseline.Files} {
			for p := range entries {
				if scoped(p, requests) && state.Selected(p) {
					candidates[p] = true
				}
			}
		}
		names := []string{}
		for p := range candidates {
			names = append(names, p)
		}
		sort.Strings(names)
		plainSource := make(map[string]history.SnapshotFile, len(names))
		plainBaseline := make(map[string]history.SnapshotFile, len(names))
		var policy secureblob.Policy
		var key []byte
		for _, p := range names {
			for _, entry := range []struct {
				snapshot history.Snapshot
				plain    map[string]history.SnapshotFile
			}{{source, plainSource}, {baseline, plainBaseline}} {
				file, ok := entry.snapshot.Files[p]
				if !ok {
					continue
				}
				if entry.snapshot.Metadata.EncryptedDigests[p] != "" {
					if key == nil {
						policy, key, err = loadRestoreKey(w)
						if err != nil {
							return err
						}
					}
					if policy.KeyFingerprint != entry.snapshot.Metadata.EncryptedKeyFingerprint || !policy.Covers(p) {
						return fmt.Errorf("%w: missing matching sensitive-file policy for %q", storage.ErrConflict, p)
					}
					identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: entry.snapshot.WorkspaceID, Path: p, Mode: file.Mode}
					var digest string
					digest, _, err = encryptedBlobDigest(ctx, r, w.Store, file.ID, key, identity)
					if err != nil {
						return err
					}
					if digest != entry.snapshot.Metadata.EncryptedDigests[p] {
						return fmt.Errorf("%w: encrypted source digest differs for %q", storage.ErrConflict, p)
					}
				}
				entry.plain[p] = file
			}
		}
		if err = validateAliases(append(append([]string{}, requests...), names...)); err != nil {
			return err
		}
		for _, p := range names {
			_, sourceFile := plainSource[p]
			_, baselineFile := plainBaseline[p]
			if sourceFile && baseline.Directories[p] || baselineFile && source.Directories[p] {
				return fmt.Errorf("%w: file/directory transition at %q", storage.ErrConflict, p)
			}
			for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
				_, sourceAncestor := source.Files[parent]
				_, baselineAncestor := baseline.Files[parent]
				if candidates[parent] || sourceAncestor || baselineAncestor {
					return fmt.Errorf("%w: file/directory transition at %q", storage.ErrConflict, parent)
				}
			}
		}
		owner, err := selection.InspectOwnership(ctx, r, w.Root, w.Admin)
		if err != nil {
			return err
		}
		canonical, err := selection.Canonical(state)
		if err != nil {
			return err
		}
		data := planData{SchemaVersion: 1, RepositoryID: w.RepositoryID, DestinationWorkspaceID: w.WorkspaceID, SourceWorkspaceID: source.WorkspaceID, SourceSnapshot: source.ID, SourceHead: sourceHead, DestinationBaseline: baselineID, Selection: canonical, Requests: requests, Ownership: owner.Evidence, Overwrite: overwrite, Parents: []Parent{}, CreateDirectories: []string{}, Operations: []Operation{}}
		published, _, err := readRestorePolicy(w)
		if err != nil {
			return err
		}
		beforeImages := map[string]Fingerprint{}
		parents := map[string]Parent{}
		createDirs := map[string]bool{}
		// One pass reads each directory once (see CheckRestorePaths).
		if err = engine.CheckRestorePaths(names); err != nil {
			return err
		}
		// A directory rule also detects main-owned descendants of a candidate
		// file; one pass folds each owned path once (see CheckPaths).
		if err = owner.CheckPaths(names); err != nil {
			return err
		}
		for _, p := range names {
			for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
				value, e := inspectParent(files, parent)
				if e != nil {
					return e
				}
				if old, ok := parents[parent]; ok && old != value {
					return fmt.Errorf("%w: parent changed at %q", storage.ErrConflict, parent)
				}
				parents[parent] = value
			}
			before, e := inspectFile(ctx, r, w.Store, files, p)
			if e != nil {
				return e
			}
			encrypted := source.Metadata.EncryptedDigests[p] != "" || baseline.Metadata.EncryptedDigests[p] != ""
			if encrypted && before.Exists {
				identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Path: p, Mode: before.GitMode}
				protected, e := inspectSensitiveFile(files, p, key, identity)
				if e != nil {
					return e
				}
				before = protectedFingerprint(protected)
			}
			beforeImages[p] = before
			desired, exists := plainSource[p]
			old, recorded := plainBaseline[p]
			matchesSource := matches(before, desired, exists)
			if encrypted {
				matchesSource = !before.Exists && !exists
				if exists {
					identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: source.WorkspaceID, Path: p, Mode: desired.Mode}
					matchesSource, e = matchesSensitiveFile(files, p, key, identity, source.Metadata.EncryptedDigests[p], true)
					if e != nil {
						return e
					}
				}
			}
			if matchesSource {
				continue
			}
			if before.Exists && !recorded {
				return fmt.Errorf("%w: unsaved obstruction at %q cannot be overwritten", storage.ErrConflict, p)
			}
			matchesBaseline := matches(before, old, recorded)
			if encrypted {
				matchesBaseline = !before.Exists && !recorded
				if recorded {
					identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Path: p, Mode: old.Mode}
					matchesBaseline, e = matchesSensitiveFile(files, p, key, identity, baseline.Metadata.EncryptedDigests[p], true)
					if e != nil {
						return e
					}
				}
			}
			dirty := !matchesBaseline
			// A missing destination has no current bytes to destroy. It may
			// differ from the baseline, but recreating it needs no overwrite grant.
			if dirty && before.Exists && !overwrite {
				return fmt.Errorf("%w: local changes at %q require --overwrite", storage.ErrConflict, p)
			}
			// Their recovery copy would be the only plaintext the private
			// store gains for a path snapshots refuse.
			if rule, ok := published.UncoveredAlias(p); ok && dirty && before.Exists {
				return fmt.Errorf("%w: local changes at %q can't be kept as a recovery copy: it matches sensitive path %q only by spelling or kind and would be saved unencrypted; move the file aside first, or correct the policy with git local encrypt migrate (see git local doctor)", storage.ErrConflict, p, rule)
			}
			op := Operation{Path: p, Action: "delete", Before: before, Dirty: dirty}
			op.Encrypted = encrypted
			if exists {
				if encrypted {
					if runtime.GOOS == "windows" && (desired.Mode == "120000" || desired.Mode == "100755") {
						return fmt.Errorf("unsupported source mode %s for Windows restore planning", desired.Mode)
					}
				} else if err = validateSourceMode(ctx, r, w.Store, source.Files[p]); err != nil {
					return err
				}
				op.Action = "create"
				if before.Exists {
					op.Action = "replace"
				}
				op.SourceBlob = source.Files[p].ID
				if source.Metadata.EncryptedDigests[p] != "" {
					op.SourceDigest = source.Metadata.EncryptedDigests[p]
				}
				op.SourceMode = desired.Mode
				op.Permissions = restorePermissions(before, desired.Mode)
				for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
					if !parents[parent].Exists {
						createDirs[parent] = true
					}
				}
			}
			if source.Metadata.EncryptedDigests[p] != "" {
				data.EncryptedKeyFingerprint = source.Metadata.EncryptedKeyFingerprint
			} else if baseline.Metadata.EncryptedDigests[p] != "" {
				data.EncryptedKeyFingerprint = baseline.Metadata.EncryptedKeyFingerprint
			}
			data.Operations = append(data.Operations, op)
		}
		for _, p := range parents {
			data.Parents = append(data.Parents, p)
		}
		sort.Slice(data.Parents, func(i, j int) bool { return data.Parents[i].Path < data.Parents[j].Path })
		for p := range createDirs {
			data.CreateDirectories = append(data.CreateDirectories, p)
		}
		sort.Strings(data.CreateDirectories)
		if beforeRecheck != nil {
			if err = beforeRecheck(); err != nil {
				return err
			}
		}
		if err = recheck(); err != nil {
			return err
		}
		for _, entry := range []struct{ id, head string }{{source.WorkspaceID, sourceHead}, {w.WorkspaceID, baselineID}} {
			h, e := history.WorkspaceHeadLocked(ctx, r, w, entry.id)
			if e != nil {
				return e
			}
			if h != entry.head {
				return fmt.Errorf("%w: snapshot head changed during planning", storage.ErrConflict)
			}
		}
		if err = owner.Revalidate(ctx, r, w.Root, w.Admin); err != nil {
			return err
		}
		if err = engine.CheckRestorePaths(names); err != nil {
			return err
		}
		for _, p := range names {
			now, e := inspectFile(ctx, r, w.Store, files, p)
			if e != nil {
				return e
			}
			if beforeImages[p].KeyedDigest != "" && now.Exists {
				identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Path: p, Mode: now.GitMode}
				protected, e := inspectSensitiveFile(files, p, key, identity)
				if e != nil {
					return e
				}
				now = protectedFingerprint(protected)
			}
			if !equalFingerprint(now, beforeImages[p]) {
				return fmt.Errorf("%w: file changed during planning at %q", storage.ErrConflict, p)
			}
		}
		for _, p := range data.Parents {
			now, e := inspectParent(files, p.Path)
			if e != nil {
				return e
			}
			if now != p {
				return fmt.Errorf("%w: parent changed during planning at %q", storage.ErrConflict, p.Path)
			}
		}
		candidate := RestorePlan{data: data}
		if _, err = candidate.JSON(); err != nil {
			return err
		}
		result = candidate
		return nil
	}()
	return result, err
}
func inspectParent(files *storage.Dir, p string) (Parent, error) {
	result := Parent{Path: p}
	fi, err := files.Lstat(p)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !fi.IsDir() {
		return result, fmt.Errorf("%w: unsafe parent %q", storage.ErrConflict, p)
	}
	result.Exists = true
	result.Permissions = uint32(fi.Mode().Perm())
	return result, nil
}
