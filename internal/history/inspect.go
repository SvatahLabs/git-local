package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type FileChange struct {
	Code string `json:"code"`
	Path string `json:"path"`
}
type SelectionChange struct {
	Code string `json:"code"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}
type StatusResult struct {
	Files     []FileChange      `json:"files"`
	Selection []SelectionChange `json:"selection"`
	Warnings  []string          `json:"warnings"`
}
type fileValue struct{ Mode, ID string }

func under(parent, path string) bool { return parent == path || strings.HasPrefix(path, parent+"/") }
func conflicted(path string, conflicts []string) bool {
	for _, p := range conflicts {
		if under(p, path) {
			return true
		}
	}
	return false
}
func selectedChanges(old, next selection.State) []SelectionChange {
	before, after := map[string]SelectionChange{}, map[string]SelectionChange{}
	fill := func(s selection.State, d map[string]SelectionChange) {
		for _, r := range s.Include {
			d[r.Kind+"\x00"+r.Path] = SelectionChange{Kind: r.Kind, Path: r.Path}
		}
		for _, p := range s.Exclude {
			d["exclude\x00"+p] = SelectionChange{Kind: "exclude", Path: p}
		}
	}
	fill(old, before)
	fill(next, after)
	result := []SelectionChange{}
	for k, v := range before {
		if _, ok := after[k]; !ok {
			v.Code = "D"
			result = append(result, v)
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			v.Code = "A"
			result = append(result, v)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Code < b.Code
	})
	return result
}
func readFiles(ctx context.Context, r *gitexec.Runner, store, treeID string) (map[string]fileValue, error) {
	result := map[string]fileValue{}
	if treeID == "" {
		return result, nil
	}
	data, err := r.HistoryListing(ctx, store, nil, "ls-tree", "-r", "-z", "--full-tree", treeID)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != 0 {
		return nil, errors.New("unterminated snapshot files tree")
	}
	aliases := map[string]string{}
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, path, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || fields[1] != "blob" || !validOID(fields[2]) || (fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000") {
			return nil, errors.New("unsupported snapshot file entry")
		}
		if err = storage.ValidateSelectionPath(path); err != nil {
			return nil, err
		}
		parts := strings.Split(path, "/")
		for i := range parts {
			p := strings.Join(parts[:i+1], "/")
			key := storage.AliasKey(p)
			if prev, ok := aliases[key]; ok && prev != p {
				return nil, errors.New("ambiguous snapshot file aliases")
			}
			aliases[key] = p
		}
		if _, ok := result[path]; ok {
			return nil, errors.New("duplicate snapshot file")
		}
		result[path] = fileValue{fields[0], fields[2]}
	}
	for path := range result {
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			if _, ok := result[strings.Join(parts[:i], "/")]; ok {
				return nil, errors.New("snapshot file/directory collision")
			}
		}
	}
	return result, nil
}
func hashRead(ctx context.Context, g historyGit, store string, input io.Reader, write bool) (string, error) {
	args := []string{"hash-object", "--stdin", "--no-filters"}
	if write {
		args = append(args, "-w")
	}
	return oid(g.History(ctx, store, input, args...))
}
func captureRead(ctx context.Context, g historyGit, store string, files *storage.Dir, plan selection.Plan, write bool) (map[string]fileValue, func() error, error) {
	result := map[string]fileValue{}
	observed := map[string]storage.Observation{}
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		mode := "100644"
		var id string
		var err error
		if entry.Mode&os.ModeSymlink != 0 {
			target, e := files.ReadLink(entry.Path)
			if e != nil {
				return nil, nil, e
			}
			if target != entry.LinkTarget {
				return nil, nil, fmt.Errorf("%w: symlink changed during inspection", storage.ErrConflict)
			}
			mode = "120000"
			id, err = hashRead(ctx, g, store, strings.NewReader(target), write)
		} else {
			var observation storage.Observation
			observation, err = files.StreamStable(entry.Path, func(input io.Reader) error { var e error; id, e = hashRead(ctx, g, store, input, write); return e })
			if err == nil {
				if observation.Mode() != entry.Mode {
					return nil, nil, fmt.Errorf("%w: file mode changed during inspection", storage.ErrConflict)
				}
				observed[entry.Path] = observation
			}
			if entry.Mode&0111 != 0 {
				mode = "100755"
			}
		}
		if err != nil {
			return nil, nil, err
		}
		result[entry.Path] = fileValue{mode, id}
	}
	recheck := func() error {
		for path, observation := range observed {
			if err := observation.Recheck(files, path); err != nil {
				return err
			}
		}
		return nil
	}
	return result, recheck, nil
}

type inspection struct {
	status        StatusResult
	before, after map[string]fileValue
	conflicts     []string
}

func inspect(ctx context.Context, r *gitexec.Runner, w workspace.Context, state selection.State, warnings []string, recheck func() error, g historyGit, store string, write bool) (inspection, error) {
	var result inspection
	result.status.Warnings = warnings
	secure, err := loadSecure(w)
	if err != nil {
		return result, err
	}
	previous, err := head(ctx, r, w.Store, Ref(w.WorkspaceID))
	if err != nil {
		return result, err
	}
	old := selection.Empty()
	oldTree := ""
	var oldMetadata Metadata
	if previous != "" {
		oldTree, oldMetadata, err = readPrevious(ctx, r, w.Store, previous, w.RepositoryID, w.WorkspaceID)
		if err != nil {
			return result, err
		}
		old = oldMetadata.Selection
	}
	result.before, err = readFiles(ctx, r, w.Store, oldTree)
	if err != nil {
		return result, err
	}
	if secure == nil && oldMetadata.SchemaVersion == 2 {
		return result, fmt.Errorf("%w: encrypted snapshot requires a sensitive-file policy", storage.ErrConflict)
	}
	if secure != nil {
		if oldMetadata.SchemaVersion == 2 && oldMetadata.EncryptedKeyFingerprint != secure.policy.KeyFingerprint {
			return result, fmt.Errorf("%w: sensitive-file key differs from prior snapshot", storage.ErrConflict)
		}
		for path := range result.before {
			if secure.covers(path) && oldMetadata.EncryptedDigests[path] == "" {
				return result, fmt.Errorf("%w: plaintext history for %q requires encryption migration", storage.ErrConflict, path)
			}
		}
		for path := range oldMetadata.EncryptedDigests {
			if _, ok := result.before[path]; !ok {
				return result, fmt.Errorf("%w: encrypted snapshot metadata references missing file %q", storage.ErrConflict, path)
			}
			if !secure.covers(path) {
				return result, fmt.Errorf("%w: encrypted history for %q is outside the sensitive-file policy", storage.ErrConflict, path)
			}
		}
	}
	engine, err := selection.New(r, w.Root, w.Admin)
	if err != nil {
		return result, err
	}
	defer engine.Close()
	plan, conflicts, err := engine.Inspect(ctx, state)
	if err != nil {
		return result, err
	}
	result.conflicts = conflicts
	files, err := storage.OpenDir(w.Root)
	if err != nil {
		return result, err
	}
	defer files.Close()
	// Never hash paths whose effective Git-ignore protection has been overridden.
	paths := []string{}
	for _, e := range plan.Entries {
		paths = append(paths, e.Path)
	}
	ignored, err := r.CheckIgnore(ctx, w.Root, w.Admin, paths)
	if err != nil {
		return result, err
	}
	safe := plan
	safe.Entries = nil
	for _, e := range plan.Entries {
		if !ignored[e.Path] {
			result.conflicts = append(result.conflicts, e.Path)
			result.status.Warnings = append(result.status.Warnings, fmt.Sprintf("Selected path %q is not effectively ignored.", e.Path))
		} else {
			safe.Entries = append(safe.Entries, e)
		}
	}
	var recheckFiles func() error
	result.after, recheckFiles, err = captureRead(ctx, g, store, files, safe, write)
	if err != nil {
		return result, err
	}
	currentDigests := map[string]string{}
	if secure != nil {
		for _, entry := range safe.Entries {
			if !secure.covers(entry.Path) {
				continue
			}
			mode := result.after[entry.Path].Mode
			identity := secure.identity(w, entry.Path, mode)
			if mode == "120000" {
				target, e := files.ReadLink(entry.Path)
				if e != nil || target != entry.LinkTarget {
					return result, fmt.Errorf("%w: symlink changed during inspection at %q", storage.ErrConflict, entry.Path)
				}
				currentDigests[entry.Path], err = secureblob.Digest(strings.NewReader(target), secure.key, identity)
			} else {
				_, err = files.StreamStable(entry.Path, func(input io.Reader) error {
					var digestErr error
					currentDigests[entry.Path], digestErr = secureblob.Digest(input, secure.key, identity)
					return digestErr
				})
			}
			if err != nil {
				return result, err
			}
		}
		if write {
			for path, digest := range oldMetadata.EncryptedDigests {
				if digest == "" {
					continue
				}
				file := result.before[path]
				plainID, e := decryptIntoTemporary(ctx, r, w.Store, g, store, file.ID, secure.key, secure.identity(w, path, file.Mode))
				if e != nil {
					return result, e
				}
				result.before[path] = fileValue{Mode: file.Mode, ID: plainID}
			}
		}
	}
	if err = recheck(); err != nil {
		return result, err
	}
	now, again, err := engine.Inspect(ctx, state)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(plan.Entries, now.Entries) || !reflect.DeepEqual(plan.Missing, now.Missing) || !reflect.DeepEqual(plan.Directories, now.Directories) || !reflect.DeepEqual(conflicts, again) {
		return result, fmt.Errorf("%w: selected paths changed during inspection", storage.ErrConflict)
	}
	if err = recheckFiles(); err != nil {
		return result, err
	}
	if err = plan.Ownership.Revalidate(ctx, r, w.Root, w.Admin); err != nil {
		return result, err
	}
	ignoredAgain, err := r.CheckIgnore(ctx, w.Root, w.Admin, paths)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(ignored, ignoredAgain) {
		return result, fmt.Errorf("%w: effective ignores changed during inspection", storage.ErrConflict)
	}
	current, err := head(ctx, r, w.Store, Ref(w.WorkspaceID))
	if err != nil {
		return result, err
	}
	if current != previous {
		return result, fmt.Errorf("%w: snapshot head changed during inspection", storage.ErrConflict)
	}
	result.status.Selection = selectedChanges(old, state)
	changes := map[string]string{}
	for path, old := range result.before {
		if conflicted(path, result.conflicts) {
			continue
		}
		now, ok := result.after[path]
		if !ok {
			changes[path] = "D"
		} else if digest := oldMetadata.EncryptedDigests[path]; digest != "" {
			if now.Mode != old.Mode || currentDigests[path] != digest {
				changes[path] = "M"
			}
		} else if now != old {
			changes[path] = "M"
		}
	}
	for path := range result.after {
		if _, ok := result.before[path]; !ok {
			changes[path] = "A"
		}
	}
	for _, path := range result.conflicts {
		changes[path] = "C"
	}
	for path, code := range changes {
		result.status.Files = append(result.status.Files, FileChange{code, path})
	}
	sort.Slice(result.status.Files, func(i, j int) bool { return result.status.Files[i].Path < result.status.Files[j].Path })
	return result, nil
}
func Status(ctx context.Context, r *gitexec.Runner, cwd string) (StatusResult, error) {
	var result StatusResult
	err := configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, s selection.State, warnings []string, recheck func() error) error {
		inspection, err := inspect(ctx, r, w, s, warnings, recheck, r, w.Store, false)
		result = inspection.status
		return err
	})
	return result, err
}

// Porcelain is v1: files first, then selection records; paths are raw NUL framed.
func (s StatusResult) Porcelain() []byte {
	var out bytes.Buffer
	for _, f := range s.Files {
		fmt.Fprintf(&out, "F\t%s\t%s%c", f.Code, f.Path, 0)
	}
	for _, r := range s.Selection {
		fmt.Fprintf(&out, "S\t%s\t%s\t%s%c", r.Code, r.Kind, r.Path, 0)
	}
	return out.Bytes()
}
func (s StatusResult) Human() string {
	var out strings.Builder
	for _, f := range s.Files {
		fmt.Fprintf(&out, "%s %q\n", f.Code, f.Path)
	}
	for _, r := range s.Selection {
		fmt.Fprintf(&out, "Selection %s %s %q\n", r.Code, r.Kind, r.Path)
	}
	if out.Len() == 0 {
		out.WriteString("No local changes.\n")
	}
	return out.String()
}
