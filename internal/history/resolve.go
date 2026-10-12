package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type SnapshotFile struct{ Mode, ID string }
type Snapshot struct {
	ID, WorkspaceID, FilesTree string
	Metadata                   Metadata
	Files                      map[string]SnapshotFile
	Directories                map[string]bool
}

func ValidateSelector(value string) error {
	if value == "latest" {
		return nil
	}
	if len(value) < 4 || len(value) > 64 {
		return errors.New("snapshot must be latest or a hexadecimal object ID/prefix of 4–64 characters")
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return errors.New("revision expressions and refs are not snapshot selectors")
		}
	}
	return nil
}
func sourceWorkspace(w workspace.Context, id string) (string, error) {
	if id == "" {
		id = w.WorkspaceID
	}
	return id, configtx.RequireWorkspace(w, id)
}

// ResolveLocked requires the caller to hold the common lock. All objects must
// belong to the chosen workspace's reachable user history in this same store.
func ResolveLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, source, selector string) (Snapshot, error) {
	var snapshot Snapshot
	if err := ValidateSelector(selector); err != nil {
		return snapshot, err
	}
	source, err := sourceWorkspace(w, source)
	if err != nil {
		return snapshot, err
	}
	tip, err := head(ctx, r, w.Store, Ref(source))
	if err != nil {
		return snapshot, err
	}
	if tip == "" {
		return snapshot, errors.New("source workspace has no snapshots")
	}
	id := tip
	if selector != "latest" {
		out, e := r.History(ctx, w.Store, nil, "rev-parse", "--disambiguate="+strings.ToLower(selector))
		if e != nil {
			return snapshot, e
		}
		ids := strings.Fields(string(out))
		if len(ids) != 1 || !validOID(ids[0]) {
			return snapshot, errors.New("snapshot ID is missing or ambiguous in this history store")
		}
		id = ids[0]
	}
	ancestry, err := r.HistoryListing(ctx, w.Store, nil, "rev-list", "--first-parent", "--parents", tip)
	if err != nil {
		return snapshot, err
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(ancestry)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 || len(fields) > 2 {
			return snapshot, errors.New("nonlinear or malformed workspace history")
		}
		for _, field := range fields {
			if !validOID(field) {
				return snapshot, errors.New("invalid history object ID")
			}
		}
		tree, m, e := readPrevious(ctx, r, w.Store, fields[0], w.RepositoryID, source)
		if e != nil {
			return snapshot, e
		}
		if fields[0] == id {
			snapshot = Snapshot{ID: id, WorkspaceID: source, FilesTree: tree, Metadata: m}
			found = true
			break
		}
	}
	if !found {
		return snapshot, errors.New("snapshot is not reachable from the chosen workspace's user head")
	}
	snapshot, err = validateSnapshotLocked(ctx, r, w, snapshot)
	if err != nil {
		return snapshot, err
	}

	current, err := head(ctx, r, w.Store, Ref(source))
	if err != nil {
		return snapshot, err
	}
	if current != tip {
		return snapshot, fmt.Errorf("%w: source history changed during resolution", storage.ErrConflict)
	}
	return snapshot, nil
}

// validateSnapshotLocked admits the complete files tree and referenced blobs of
// one user snapshot. It is also used by the full history audit.
func validateSnapshotLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, snapshot Snapshot) (Snapshot, error) {
	files, err := readFiles(ctx, r, w.Store, snapshot.FilesTree)
	if err != nil {
		return snapshot, err
	}
	secure, err := loadSecure(w)
	if err != nil {
		return snapshot, err
	}
	if secure == nil && snapshot.Metadata.SchemaVersion == 2 {
		return snapshot, errors.New("encrypted snapshot requires a sensitive-file policy")
	}
	if secure != nil && snapshot.Metadata.SchemaVersion == 2 && secure.policy.KeyFingerprint != snapshot.Metadata.EncryptedKeyFingerprint {
		return snapshot, errors.New("sensitive-file key differs from snapshot metadata")
	}
	snapshot.Files = map[string]SnapshotFile{}
	snapshot.Directories = map[string]bool{}
	for path, file := range files {
		if secure.covers(path) != (snapshot.Metadata.EncryptedDigests[path] != "") {
			return snapshot, errors.New("sensitive-file policy and snapshot content disagree")
		}
		if !snapshot.Metadata.Selection.Selected(path) {
			return snapshot, errors.New("snapshot content lies outside its recorded selection")
		}
		for _, rule := range snapshot.Metadata.Selection.Include {
			if rule.Kind == "directory" && rule.Path == path {
				return snapshot, errors.New("snapshot selection kind conflicts with stored file")
			}
		}
		snapshot.Files[path] = SnapshotFile(file)
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			snapshot.Directories[strings.Join(parts[:i], "/")] = true
		}
	}
	for path := range snapshot.Metadata.EncryptedDigests {
		if _, ok := files[path]; !ok {
			return snapshot, errors.New("encrypted snapshot metadata references a missing file")
		}
	}
	// Validate tree entries as well: recursive file-only enumeration hides empty or
	// ambiguous directories, which the snapshot schema does not admit.
	trees, err := r.HistoryListing(ctx, w.Store, nil, "ls-tree", "-r", "-t", "-z", "--full-tree", snapshot.FilesTree)
	if err != nil {
		return snapshot, err
	}
	seen := map[string]bool{}
	for _, entry := range bytes.Split(trees, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		header, path, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || !validOID(fields[2]) {
			return snapshot, errors.New("malformed historical tree")
		}
		if err = storage.ValidateSelectionPath(path); err != nil {
			return snapshot, err
		}
		if seen[path] {
			return snapshot, errors.New("duplicate historical tree entry")
		}
		seen[path] = true
		if fields[1] == "tree" {
			if fields[0] != "040000" || !snapshot.Directories[path] {
				return snapshot, errors.New("empty or unsupported historical directory")
			}
		} else if _, ok := snapshot.Files[path]; !ok {
			return snapshot, errors.New("unsupported historical file")
		}
	}
	ids := []string{}
	unique := map[string]bool{}
	for _, file := range snapshot.Files {
		if !unique[file.ID] {
			unique[file.ID] = true
			ids = append(ids, file.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		out, e := r.HistoryListing(ctx, w.Store, strings.NewReader(strings.Join(ids, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
		if e != nil {
			return snapshot, e
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != len(ids) {
			return snapshot, errors.New("missing historical objects")
		}
		for i, line := range lines {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[0] != ids[i] || fields[1] != "blob" {
				return snapshot, errors.New("missing or unsupported historical blob")
			}
			size, e := strconv.ParseInt(fields[2], 10, 64)
			if e != nil || size < 0 {
				return snapshot, errors.New("invalid historical blob size")
			}
		}
	}
	return snapshot, nil
}

func Resolve(ctx context.Context, r *gitexec.Runner, cwd, source, selector string) (Snapshot, error) {
	var snapshot Snapshot
	err := configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		var e error
		snapshot, e = ResolveLocked(ctx, r, w, source, selector)
		if e != nil {
			return e
		}
		return recheck()
	})
	return snapshot, err
}
func (s Snapshot) rule(path string) (selection.Rule, error) {
	if s.Metadata.Selection.Excluded(path) {
		return selection.Rule{}, fmt.Errorf("historical path %q is excluded in the source snapshot", path)
	}
	if _, ok := s.Files[path]; ok {
		return selection.Rule{Path: path, Kind: "file"}, nil
	}
	if s.Directories[path] {
		return selection.Rule{Path: path, Kind: "directory"}, nil
	}
	for _, rule := range s.Metadata.Selection.Include {
		if rule.Path == path {
			return rule, nil
		}
	}
	return selection.Rule{}, fmt.Errorf("historical path %q has no unambiguous recorded type", path)
}
func TrackFrom(ctx context.Context, r *gitexec.Runner, cwd, source, selector string, paths ...string) (configtx.Result, error) {
	if err := ValidateSelector(selector); err != nil {
		return configtx.Result{}, err
	}
	return configtx.ChangePlanned(ctx, r, cwd, func(w workspace.Context, state selection.State) (selection.State, error) {
		snapshot, err := ResolveLocked(ctx, r, w, source, selector)
		if err != nil {
			return selection.State{}, err
		}
		engine, err := selection.New(r, w.Root, w.Admin)
		if err != nil {
			return selection.State{}, err
		}
		defer engine.Close()
		rules := []selection.Rule{}
		for _, arg := range paths {
			path, e := engine.Normalize(cwd, arg)
			if e != nil {
				return selection.State{}, e
			}
			rule, e := snapshot.rule(path)
			if e != nil {
				return selection.State{}, e
			}
			rules = append(rules, rule)
		}
		plan, err := engine.PlanHistorical(ctx, state, rules)
		if err != nil {
			return selection.State{}, err
		}
		return plan.State, nil
	})
}

// WorkspaceHeadLocked returns an optional head under the caller's repository lock.
func WorkspaceHeadLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, id string) (string, error) {
	source, err := sourceWorkspace(w, id)
	if err != nil {
		return "", err
	}
	return head(ctx, r, w.Store, Ref(source))
}
