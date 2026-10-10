package configtx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitlocal/internal/storage"
)

// QuarantineDir holds bytes an abandoned transaction would otherwise delete
// without another preserved copy. It lives in <common>/gitlocal and is kept
// until the user deletes it.
const QuarantineDir = "quarantine"

// QuarantineEntry names one preserved item. File is relative to the
// transaction's quarantine directory.
type QuarantineEntry struct {
	File   string `json:"file"`
	Source string `json:"source"`
	Root   string `json:"root,omitempty"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// Quarantine is the manifest written as quarantine/<id>/manifest.json.
type Quarantine struct {
	SchemaVersion int               `json:"schemaVersion"`
	TransactionID string            `json:"transactionId"`
	Kind          string            `json:"kind"`
	Entries       []QuarantineEntry `json:"entries"`
}

// AbandonResult reports an abandoned transaction. Quarantine is the absolute
// quarantine directory, or empty when nothing needed preserving.
type AbandonResult struct {
	TransactionID string
	Kind          string
	Quarantine    string
	Quarantined   []QuarantineEntry
	Removed       []string
	Warnings      []string
	// ExcludeRewritten is true when abandon replaced the managed block in
	// info/exclude.
	ExcludeRewritten bool
}

func quarantinePath(id string) string { return path.Join(QuarantineDir, id) }

// NewQuarantine starts or continues the quarantine manifest of id. An earlier
// interrupted abandon keeps its entries.
func NewQuarantine(meta *storage.Dir, id, kind string) (*Quarantine, error) {
	q := &Quarantine{SchemaVersion: 1, TransactionID: id, Kind: kind, Entries: []QuarantineEntry{}}
	err := meta.ReadJSON(path.Join(quarantinePath(id), "manifest.json"), q)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if q.SchemaVersion != 1 || q.TransactionID != id || q.Kind != kind {
		return nil, fmt.Errorf("%w: quarantine %s belongs to another transaction", storage.ErrConflict, id)
	}
	return q, nil
}

func (q *Quarantine) add(entry QuarantineEntry) {
	for i, old := range q.Entries {
		if old.File == entry.File {
			q.Entries[i] = entry
			return
		}
	}
	q.Entries = append(q.Entries, entry)
}

// Save writes the manifest. It is a no-op while nothing is quarantined.
func (q *Quarantine) Save(meta *storage.Dir) error {
	if len(q.Entries) == 0 {
		return nil
	}
	if err := meta.MkdirAll(quarantinePath(q.TransactionID)); err != nil {
		return err
	}
	return meta.WriteJSON(path.Join(quarantinePath(q.TransactionID), "manifest.json"), q)
}

// Directory returns the absolute quarantine path, or "" when it is empty.
func (q *Quarantine) Directory(common string) string {
	if len(q.Entries) == 0 {
		return ""
	}
	return filepath.Join(common, "gitlocal", filepath.FromSlash(quarantinePath(q.TransactionID)))
}

// QuarantineFile copies a reserved staging file from files into the quarantine
// and removes it only after the copy is durable. A symlink is preserved as a
// regular file holding its target. Anything else is refused untouched.
func (q *Quarantine) QuarantineFile(meta, files *storage.Dir, name, root, reason string) (QuarantineEntry, error) {
	token := strings.TrimPrefix(path.Base(name), ".gitlocal-tmp-")
	if !storage.IsTempName(path.Base(name)) {
		return QuarantineEntry{}, fmt.Errorf("refusing to quarantine non-staging path %q", name)
	}
	info, err := files.Lstat(name)
	if err != nil {
		return QuarantineEntry{}, err
	}
	entry := QuarantineEntry{File: path.Join("files", token), Source: name, Root: root, Kind: "file", Reason: reason}
	if err := meta.MkdirAll(path.Join(quarantinePath(q.TransactionID), "files")); err != nil {
		return entry, err
	}
	target := path.Join(quarantinePath(q.TransactionID), entry.File)
	// A copy left by an interrupted abandon is incomplete while its source
	// still exists; replace it.
	if _, err := meta.Lstat(target); err == nil {
		if err := meta.Remove(target); err != nil {
			return entry, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return entry, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, e := files.ReadLink(name)
		if e != nil {
			return entry, e
		}
		entry.Kind = "symlink"
		err = meta.StageNamed(target, 0600, func(out io.Writer) error { _, e := io.WriteString(out, link); return e })
	case info.Mode().IsRegular():
		_, err = files.StreamStable(name, func(input io.Reader) error {
			return meta.StageNamed(target, 0600, func(out io.Writer) error { _, e := io.Copy(out, input); return e })
		})
	default:
		return entry, fmt.Errorf("%w: unexpected entry at staging path %q; inspect it manually", storage.ErrConflict, name)
	}
	if err != nil {
		return entry, err
	}
	if err := meta.SyncStaged(target); err != nil {
		return entry, err
	}
	q.add(entry)
	if err := q.Save(meta); err != nil {
		return entry, err
	}
	return entry, files.Remove(name)
}

// QuarantineJournal moves a completed journal into the quarantine, under
// journals/. Both live in <common>/gitlocal, so the move is a rename.
func (q *Quarantine) QuarantineJournal(meta *storage.Dir, name, reason string) (QuarantineEntry, error) {
	entry := QuarantineEntry{File: path.Join("journals", path.Base(name)), Source: name, Kind: "journal", Reason: reason}
	if path.Dir(name) != "transactions/completed" || !strings.HasSuffix(name, ".json") {
		return entry, fmt.Errorf("refusing to quarantine %q", name)
	}
	if err := meta.MkdirAll(path.Join(quarantinePath(q.TransactionID), "journals")); err != nil {
		return entry, err
	}
	q.add(entry)
	if err := q.Save(meta); err != nil {
		return entry, err
	}
	return entry, meta.Rename(name, path.Join(quarantinePath(q.TransactionID), entry.File))
}

// HoldsJournal reports whether this quarantine already holds the completed
// journal name (transactions/completed/...): an interrupted abandon recorded
// and moved it, so a rerun must not expect it in its old place.
func (q *Quarantine) HoldsJournal(meta *storage.Dir, name string) bool {
	for _, entry := range q.Entries {
		if entry.Kind == "journal" && entry.Source == name {
			_, err := meta.Lstat(path.Join(quarantinePath(q.TransactionID), entry.File))
			return err == nil
		}
	}
	return false
}

// QuarantineStore moves a directory owned by an encryption migration from
// <common>/gitlocal into the quarantine. Both live in the same directory, so
// the move is a rename and never copies or deletes bytes.
func (q *Quarantine) QuarantineStore(meta *storage.Dir, name, reason string) (QuarantineEntry, error) {
	entry := QuarantineEntry{File: name, Source: name, Kind: "store", Reason: reason}
	if strings.ContainsAny(name, "/\\") || !strings.HasPrefix(name, "history-") {
		return entry, fmt.Errorf("refusing to quarantine %q", name)
	}
	if err := meta.MkdirAll(quarantinePath(q.TransactionID)); err != nil {
		return entry, err
	}
	q.add(entry)
	if err := q.Save(meta); err != nil {
		return entry, err
	}
	return entry, meta.Rename(name, path.Join(quarantinePath(q.TransactionID), name))
}
