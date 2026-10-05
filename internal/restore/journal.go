package restore

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

const journalLimit = 16 << 20

type stagePaths struct {
	Apply    string `json:"apply"`
	Rollback string `json:"rollback"`
}
type manifest struct {
	Stages        []stagePaths  `json:"stages"`
	BeforeStored  []string      `json:"beforeStored,omitempty"`
	PostStored    []string      `json:"postStored,omitempty"`
	SchemaVersion int           `json:"schemaVersion"`
	Kind          string        `json:"kind"`
	ID            string        `json:"id"`
	Plan          planData      `json:"plan"`
	ConfigDigest  string        `json:"configDigest"`
	MainBranch    string        `json:"mainBranch"`
	Post          []Fingerprint `json:"post"`
}
type journal struct {
	SchemaVersion    int      `json:"schemaVersion"`
	Kind             string   `json:"kind"`
	ID               string   `json:"id"`
	RecoverySnapshot string   `json:"recoverySnapshot"`
	Manifest         manifest `json:"manifest"`
	Direction        string   `json:"direction"`
	Outcome          string   `json:"outcome,omitempty"`
	Progress         int      `json:"progress"`
}
type Result struct {
	TransactionID    string
	RecoverySnapshot string
	Changed          bool
}
type fault func(string) error

func point(ctx context.Context, f fault, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f != nil {
		return f(name)
	}
	return nil
}
func stopped(id string, err error) error {
	return fmt.Errorf("%w: restore transaction %s stopped (%v); run git local recover %s --resume or --rollback", storage.ErrConflict, id, err, id)
}
func journalPath(id string) string   { return "transactions/restore-" + id + ".json" }
func completedPath(id string) string { return "transactions/completed/restore-" + id + ".json" }
func RecoveryRef(workspace, id string) string {
	return "refs/gitlocal/recovery/" + workspace + "/" + id
}
func encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > journalLimit {
		return nil, errors.New("restore journal exceeds metadata limit")
	}
	return append(b, '\n'), nil
}
func readJournal(meta *storage.Dir, name string) (journal, error) {
	var j journal
	b, err := meta.ReadFile(name, journalLimit)
	if err != nil {
		return j, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&j); err != nil {
		return j, err
	}
	if d.Decode(new(any)) != io.EOF {
		return j, errors.New("trailing restore journal metadata")
	}
	return j, nil
}
func writeJournal(meta *storage.Dir, j journal) error {
	b, err := encode(j)
	if err != nil {
		return err
	}
	return meta.AtomicWrite(journalPath(j.ID), b, 0600)
}
func oid(b []byte, err error) (string, error) {
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if !validOID(id) {
		return "", errors.New("invalid restore object ID")
	}
	return id, nil
}
func validOID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func hashBlob(ctx context.Context, r *gitexec.Runner, store string, input io.Reader) (string, error) {
	return oid(r.History(ctx, store, input, "hash-object", "-w", "--stdin", "--no-filters"))
}

type countWriter int64

func (w *countWriter) Write(b []byte) (int, error) { *w += countWriter(len(b)); return len(b), nil }
func blobFingerprint(ctx context.Context, r *gitexec.Runner, store, id, mode string, permissions uint32) (Fingerprint, error) {
	b, err := r.History(ctx, store, nil, "cat-file", "-s", id)
	if err != nil {
		return Fingerprint{}, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || size < 0 {
		return Fingerprint{}, errors.New("invalid blob size")
	}
	var gitHash hash.Hash = sha1.New()
	if len(id) == 64 {
		gitHash = sha256.New()
	}
	fmt.Fprintf(gitHash, "blob %d%c", size, 0)
	digest := sha256.New()
	var count countWriter
	if err = r.HistoryBlob(ctx, store, id, io.MultiWriter(gitHash, digest, &count)); err != nil {
		return Fingerprint{}, err
	}
	if int64(count) != size || hex.EncodeToString(gitHash.Sum(nil)) != id {
		return Fingerprint{}, errors.New("corrupt restore blob")
	}
	kind := "file"
	if mode == "120000" {
		kind = "symlink"
		permissions = 0777
	}
	return Fingerprint{Exists: true, Kind: kind, GitMode: mode, BlobID: id, SHA256: hex.EncodeToString(digest.Sum(nil)), Size: size, Permissions: permissions}, nil
}
func snapshotEntries(m manifest, manifestID string) string {
	var b strings.Builder
	// mktree/ls-tree use byte order; after, before, manifest is canonical.
	for i, im := range m.Post {
		if im.Exists {
			stored := im.BlobID
			if m.SchemaVersion == 2 {
				stored = m.PostStored[i]
			}
			fmt.Fprintf(&b, "100644 blob %s\tafter-%08d%c", stored, i, 0)
		}
	}
	for i, op := range m.Plan.Operations {
		if op.Before.Exists {
			stored := op.Before.BlobID
			if m.SchemaVersion == 2 {
				stored = m.BeforeStored[i]
			}
			fmt.Fprintf(&b, "100644 blob %s\tbefore-%08d%c", stored, i, 0)
		}
	}
	fmt.Fprintf(&b, "100644 blob %s\tmanifest.json%c", manifestID, 0)
	return b.String()
}
func anchor(ctx context.Context, r *gitexec.Runner, store string, m manifest, f fault) (string, error) {
	data, err := encode(m)
	if err != nil {
		return "", err
	}
	if err = point(ctx, f, "before-manifest-blob"); err != nil {
		return "", err
	}
	manifestID, err := hashBlob(ctx, r, store, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if err = point(ctx, f, "manifest-blob"); err != nil {
		return "", err
	}
	if err = point(ctx, f, "before-recovery-tree"); err != nil {
		return "", err
	}
	tree, err := oid(r.History(ctx, store, strings.NewReader(snapshotEntries(m, manifestID)), "mktree", "-z"))
	if err != nil {
		return "", err
	}
	if err = point(ctx, f, "recovery-tree"); err != nil {
		return "", err
	}
	if err = point(ctx, f, "before-recovery-commit"); err != nil {
		return "", err
	}
	commit, err := oid(r.History(ctx, store, strings.NewReader("Git Local restore recovery "+m.ID+"\n"), "commit-tree", tree))
	if err != nil {
		return "", err
	}
	if err = point(ctx, f, "recovery-commit"); err != nil {
		return "", err
	}
	if err = point(ctx, f, "before-recovery-ref"); err != nil {
		return "", err
	}
	_, err = r.History(ctx, store, nil, "update-ref", "--no-deref", RecoveryRef(m.Plan.DestinationWorkspaceID, m.ID), commit, strings.Repeat("0", len(commit)))
	if err != nil {
		return "", err
	}
	if err = point(ctx, f, "recovery-ref"); err != nil {
		return "", err
	}
	return commit, nil
}
func verifyAnchor(ctx context.Context, r *gitexec.Runner, store string, j journal) error {
	ref := RecoveryRef(j.Manifest.Plan.DestinationWorkspaceID, j.ID)
	if _, err := r.History(ctx, store, nil, "symbolic-ref", "--quiet", ref); err == nil {
		return errors.New("symbolic recovery ref")
	} else {
		var ge *gitexec.Error
		if !errors.As(err, &ge) || ge.Code != 1 {
			return err
		}
	}
	id, err := oid(r.History(ctx, store, nil, "rev-parse", "--verify", ref))
	if err != nil {
		return err
	}
	if id != j.RecoverySnapshot {
		return errors.New("recovery anchor changed")
	}
	kind, err := r.History(ctx, store, nil, "cat-file", "-t", id)
	if err != nil {
		return err
	}
	if string(kind) != "commit\n" {
		return errors.New("recovery anchor is not a commit")
	}
	data, err := encode(j.Manifest)
	if err != nil {
		return err
	}
	manifestID, err := oid(r.History(ctx, store, bytes.NewReader(data), "hash-object", "--stdin", "--no-filters"))
	if err != nil {
		return err
	}
	entries, err := r.History(ctx, store, nil, "ls-tree", "-z", id)
	if err != nil {
		return err
	}
	if string(entries) != snapshotEntries(j.Manifest, manifestID) {
		return errors.New("journal does not match anchored recovery manifest")
	}
	// Prove the manifest blob itself is present and intact, not just named by the tree.
	actual, err := r.History(ctx, store, nil, "cat-file", "blob", manifestID)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, data) {
		return errors.New("corrupt recovery manifest")
	}
	return nil
}
func openMeta(common string) (*storage.Dir, error) {
	return storage.OpenDir(filepath.Join(common, "gitlocal"))
}
func archive(ctx context.Context, meta *storage.Dir, j journal, f fault) error {
	if err := point(ctx, f, "before-archive"); err != nil {
		return err
	}
	if err := meta.MkdirAll("transactions/completed"); err != nil {
		return err
	}
	if _, err := meta.Lstat(completedPath(j.ID)); err == nil {
		return errors.New("completed restore journal already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := meta.Rename(journalPath(j.ID), completedPath(j.ID)); err != nil {
		return err
	}
	return point(ctx, f, "archived")
}

// InspectJournal checks a restore journal under an existing shared lock. A
// journal's destination must be a known active or retained workspace. Foreign
// pending journals get full immutable-evidence validation, while live selection
// checks remain scoped to the invoking destination workspace. It does not
// replay, archive or acquire a mutation lock.
func InspectJournal(ctx context.Context, r *gitexec.Runner, w workspace.Context, state selection.State, digest, id string, completed bool, knownWorkspaces []string) error {
	meta, err := openMeta(w.Common)
	if err != nil {
		return err
	}
	defer meta.Close()
	name := journalPath(id)
	if completed {
		name = completedPath(id)
	}
	j, err := readJournal(meta, name)
	if err != nil {
		return err
	}
	if completed && j.Outcome == "" {
		return errors.New("completed restore journal has no outcome")
	}
	destination := j.Manifest.Plan.DestinationWorkspaceID
	known := false
	for _, workspaceID := range knownWorkspaces {
		if workspaceID == destination {
			known = true
			break
		}
	}
	if !known {
		return errors.New("restore journal destination workspace is unknown")
	}
	activeDestination := destination == w.WorkspaceID
	w.WorkspaceID = destination
	return validateJournalWithLiveState(ctx, r, w, state, digest, id, j, activeDestination)
}
