// Package history owns snapshots exclusively in the independent history store.
package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
)

type historyGit interface {
	History(context.Context, string, io.Reader, ...string) ([]byte, error)
}

type Metadata struct {
	SchemaVersion           int               `json:"schemaVersion"`
	Kind                    string            `json:"kind"`
	RepositoryID            string            `json:"repositoryId"`
	WorkspaceID             string            `json:"workspaceId"`
	Selection               selection.State   `json:"selection"`
	MainHead                string            `json:"mainHead,omitempty"`
	MainBranch              string            `json:"mainBranch,omitempty"`
	Writer                  string            `json:"writer,omitempty"`
	Scope                   []string          `json:"scope,omitempty"`
	ChangedPaths            []string          `json:"changedPaths,omitempty"`
	EncryptedKeyFingerprint string            `json:"encryptedKeyFingerprint,omitempty"`
	EncryptedDigests        map[string]string `json:"encryptedDigests,omitempty"`
}

func Ref(workspaceID string) string { return "refs/gitlocal/workspaces/" + workspaceID + "/head" }

const (
	// maxInlineMetadata is the metadata.json limit; every earlier release
	// reads snapshots within it.
	maxInlineMetadata    = 1 << 20
	changedPathsBlob     = "changed-paths"
	encryptedDigestsBlob = "encrypted-digests.json"
)

// maxListBlob bounds a changed-path list or digest map stored in its own
// wrapper blob, when written and when read.
var maxListBlob = 256 << 20

// writeWrapper stores a snapshot's files tree and metadata. Metadata that
// fits maxInlineMetadata stays in metadata.json, as earlier releases wrote it.
// Otherwise the changed paths (NUL-terminated) and the encrypted digests (a
// JSON object) move into their own blobs, so neither the number of changed
// files nor of sensitive files limits a snapshot.
func writeWrapper(ctx context.Context, r historyGit, store, filesTree string, m Metadata) (string, error) {
	entries := []treeEntry{{"040000", "tree", filesTree, "files"}}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if len(data)+1 > maxInlineMetadata {
		if len(m.ChangedPaths) > 0 {
			var list bytes.Buffer
			for _, path := range m.ChangedPaths {
				list.WriteString(path)
				list.WriteByte(0)
			}
			if list.Len() > maxListBlob {
				return "", errors.New("snapshot changed-path list exceeds size limit")
			}
			id, err := hash(ctx, r, store, &list)
			if err != nil {
				return "", err
			}
			entries = append(entries, treeEntry{"100644", "blob", id, changedPathsBlob})
		}
		if len(m.EncryptedDigests) > 0 {
			digests, err := json.Marshal(m.EncryptedDigests)
			if err != nil {
				return "", err
			}
			if len(digests)+1 > maxListBlob {
				return "", errors.New("snapshot encrypted digests exceed size limit")
			}
			id, err := hash(ctx, r, store, bytes.NewReader(append(digests, '\n')))
			if err != nil {
				return "", err
			}
			entries = append(entries, treeEntry{"100644", "blob", id, encryptedDigestsBlob})
		}
		m.ChangedPaths, m.EncryptedDigests = nil, nil
		if data, err = json.Marshal(m); err != nil {
			return "", err
		}
	}
	data = append(data, '\n')
	if len(data) > maxInlineMetadata {
		return "", errors.New("snapshot metadata exceeds size limit")
	}
	metadataID, err := hash(ctx, r, store, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	return tree(ctx, r, store, append(entries, treeEntry{"100644", "blob", metadataID, "metadata.json"}))
}

// readWrapperBlob reads a wrapper blob of at most limit bytes. It streams the
// blob rather than using the bounded output of a history command, whose cap is
// smaller than maxListBlob.
func readWrapperBlob(ctx context.Context, r *gitexec.Runner, store, id string, limit int64) ([]byte, error) {
	size, err := r.History(ctx, store, nil, "cat-file", "-s", id)
	if err != nil {
		return nil, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
	if err != nil || n < 0 || n > limit {
		return nil, errors.New("snapshot metadata exceeds size limit")
	}
	var out bytes.Buffer
	out.Grow(int(n))
	if err := r.HistoryBlobTo(ctx, store, id, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func oid(out []byte, err error) (string, error) {
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(string(out), "\n")
	if !validOID(s) {
		return "", errors.New("invalid history object ID")
	}
	return s, nil
}
func hash(ctx context.Context, r historyGit, store string, input io.Reader) (string, error) {
	return oid(r.History(ctx, store, input, "hash-object", "-w", "--stdin", "--no-filters"))
}

type treeEntry struct{ mode, kind, id, name string }

func tree(ctx context.Context, r historyGit, store string, entries []treeEntry) (string, error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var input bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&input, "%s %s %s\t%s%c", e.mode, e.kind, e.id, e.name, 0)
	}
	return oid(r.History(ctx, store, &input, "mktree", "-z"))
}

type directory struct {
	dirs  map[string]*directory
	files []treeEntry
}

func (d *directory) add(path, mode, id string) {
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		if d.dirs == nil {
			d.dirs = map[string]*directory{}
		}
		if d.dirs[part] == nil {
			d.dirs[part] = &directory{}
		}
		d = d.dirs[part]
	}
	d.files = append(d.files, treeEntry{mode, "blob", id, parts[len(parts)-1]})
}
func (d *directory) write(ctx context.Context, r historyGit, store string) (string, error) {
	return d.writeExcept(ctx, r, store, "")
}

type treeRecord struct {
	id      string
	entries []treeEntry
}

// plan appends d's trees, children first, with the IDs Git gives them.
func (d *directory) plan(records *[]treeRecord) (string, error) {
	entries := append([]treeEntry{}, d.files...)
	for name, child := range d.dirs {
		id, err := child.plan(records)
		if err != nil {
			return "", err
		}
		entries = append(entries, treeEntry{"040000", "tree", id, name})
	}
	id, err := treeID(entries)
	if err != nil {
		return "", err
	}
	*records = append(*records, treeRecord{id, entries})
	return id, nil
}

// writeExcept writes d's trees in one Git process, or none when d's tree is
// known: an existing tree's subtrees exist too (GL-135).
func (d *directory) writeExcept(ctx context.Context, r historyGit, store, known string) (string, error) {
	var records []treeRecord
	root, err := d.plan(&records)
	if err != nil {
		return "", err
	}
	if root == known {
		return root, nil
	}
	if len(records) == 1 && len(records[0].entries) == 0 {
		return tree(ctx, r, store, nil)
	}
	var input bytes.Buffer
	for _, record := range records {
		for _, e := range record.entries {
			fmt.Fprintf(&input, "%s %s %s\t%s%c", e.mode, e.kind, e.id, e.name, 0)
		}
		input.WriteByte(0)
	}
	output, err := r.History(ctx, store, &input, "mktree", "-z", "--batch")
	if err != nil {
		return "", err
	}
	ids := strings.Split(string(output), "\n")
	if len(ids) != len(records)+1 || ids[len(records)] != "" {
		return "", errors.New("invalid batched tree IDs")
	}
	for i, record := range records {
		if ids[i] != record.id {
			return "", fmt.Errorf("git wrote tree %s where %s was expected", ids[i], record.id)
		}
	}
	return root, nil
}
func head(ctx context.Context, r historyGit, store, ref string) (string, error) {
	// Symbolic history heads are not part of the snapshot schema.
	_, err := r.History(ctx, store, nil, "symbolic-ref", "--quiet", ref)
	var ge *gitexec.Error
	if err == nil {
		return "", errors.New("symbolic history head is unsupported")
	}
	if !errors.As(err, &ge) || ge.Code != 1 {
		return "", err
	}
	_, err = r.History(ctx, store, nil, "show-ref", "--verify", "--quiet", ref)
	if err != nil {
		if errors.As(err, &ge) && ge.Code == 1 {
			return "", nil
		}
		return "", err
	}
	return oid(r.History(ctx, store, nil, "rev-parse", "--verify", ref))
}
func readPrevious(ctx context.Context, r *gitexec.Runner, store, id, repo, workspace string) (string, Metadata, error) {
	var m Metadata
	kind, err := r.History(ctx, store, nil, "cat-file", "-t", id)
	if err != nil {
		return "", m, err
	}
	if string(kind) != "commit\n" {
		return "", m, errors.New("history head is not a commit")
	}
	// The wrapper tree has at most four entries, so this listing stays capped.
	out, err := r.History(ctx, store, nil, "ls-tree", "-z", id)
	if err != nil {
		return "", m, err
	}
	records := bytes.Split(out, []byte{0})
	if len(records) < 3 || len(records) > 5 || len(records[len(records)-1]) != 0 {
		return "", m, errors.New("invalid snapshot wrapper tree")
	}
	files, metadata, changed, digests := "", "", "", ""
	for _, record := range records[:len(records)-1] {
		prefix, name, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(prefix)
		if !ok || len(fields) != 3 || !validOID(fields[2]) {
			return "", m, errors.New("invalid snapshot tree entry")
		}
		switch {
		case name == "files" && fields[0] == "040000" && fields[1] == "tree" && files == "":
			files = fields[2]
		case name == "metadata.json" && fields[0] == "100644" && fields[1] == "blob" && metadata == "":
			metadata = fields[2]
		case name == changedPathsBlob && fields[0] == "100644" && fields[1] == "blob" && changed == "":
			changed = fields[2]
		case name == encryptedDigestsBlob && fields[0] == "100644" && fields[1] == "blob" && digests == "":
			digests = fields[2]
		default:
			return "", m, errors.New("invalid snapshot wrapper entry")
		}
	}
	if files == "" || metadata == "" {
		return "", m, errors.New("incomplete snapshot wrapper")
	}
	data, err := readWrapperBlob(ctx, r, store, metadata, maxInlineMetadata)
	if err != nil {
		return "", m, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return "", m, errors.New("invalid snapshot metadata")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "", m, errors.New("trailing snapshot metadata")
	}
	// A list kept in its own blob must not also be inline.
	if changed != "" {
		list, err := readWrapperBlob(ctx, r, store, changed, int64(maxListBlob))
		if err != nil {
			return "", m, err
		}
		if len(m.ChangedPaths) != 0 || len(list) == 0 || list[len(list)-1] != 0 {
			return "", m, errors.New("invalid snapshot changed-path list")
		}
		m.ChangedPaths = strings.Split(string(list[:len(list)-1]), "\x00")
	}
	if digests != "" {
		data, err := readWrapperBlob(ctx, r, store, digests, int64(maxListBlob))
		if err != nil {
			return "", m, err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var loaded map[string]string
		if len(m.EncryptedDigests) != 0 || decoder.Decode(&loaded) != nil || decoder.Decode(new(any)) != io.EOF || len(loaded) == 0 {
			return "", m, errors.New("invalid snapshot encrypted digests")
		}
		m.EncryptedDigests = loaded
	}
	if (m.SchemaVersion != 1 && m.SchemaVersion != 2) || m.Kind != "user" || m.RepositoryID != repo || m.WorkspaceID != workspace || !storage.ValidUUID(workspace) || m.MainHead != "" && !validOID(m.MainHead) {
		return "", m, errors.New("snapshot identity or schema mismatch")
	}
	if m.SchemaVersion == 1 && (m.EncryptedKeyFingerprint != "" || len(m.EncryptedDigests) != 0) {
		return "", m, errors.New("encrypted metadata requires snapshot schema 2")
	}
	if m.SchemaVersion == 2 && !validDigest(m.EncryptedKeyFingerprint) {
		return "", m, errors.New("invalid snapshot encryption key fingerprint")
	}
	for path, digest := range m.EncryptedDigests {
		if storage.ValidateSelectionPath(path) != nil || !m.Selection.Selected(path) || !validDigest(digest) {
			return "", m, errors.New("invalid encrypted snapshot file metadata")
		}
	}
	if err := ValidateWriter(m.Writer); err != nil {
		return "", m, err
	}
	for i, path := range m.Scope {
		if err := storage.ValidateSelectionPath(path); err != nil {
			return "", m, err
		}
		if i > 0 && (m.Scope[i-1] >= path || under(m.Scope[i-1], path)) {
			return "", m, errors.New("invalid snapshot scope ordering")
		}
		if !m.Selection.Selected(path) {
			return "", m, errors.New("snapshot scope lies outside selection")
		}
	}
	for i, path := range m.ChangedPaths {
		if err := storage.ValidateSelectionPath(path); err != nil {
			return "", m, err
		}
		if i > 0 && m.ChangedPaths[i-1] >= path {
			return "", m, errors.New("invalid changed-path ordering")
		}
		// Outside its scope a scoped snapshot only drops paths its own
		// selection no longer holds (GL-137).
		if len(m.Scope) != 0 && !inScope(m.Scope, path) && m.Selection.Selected(path) {
			return "", m, errors.New("scoped snapshot changed a path outside its scope")
		}
	}
	m.Selection, err = selection.Canonical(m.Selection)
	return files, m, err
}

// advance never follows symbolic refs and requires the exact previously read value.
func advance(ctx context.Context, r historyGit, store, ref, next, old string) error {
	_, err := r.History(ctx, store, nil, "update-ref", "--no-deref", ref, next, old)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Cancellation may arrive after Git updated the ref but before its
			// process result reached us. Inspect the ref with a fresh, bounded
			// context so callers can report an already published snapshot.
			checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			current, checkErr := head(checkCtx, r, store, ref)
			cancel()
			if checkErr == nil && current == next {
				return nil
			}
			if checkErr == nil && current == old {
				return err
			}
		}
		return fmt.Errorf("%w: history ref publication failed; inspect the current head before retrying: %v", storage.ErrConflict, err)
	}
	return nil
}
