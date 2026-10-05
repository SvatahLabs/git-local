// Package configtx serializes selection and shared-ignore configuration updates.
// Journals are metadata only; no working file contents enter this package.
package configtx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type Image struct {
	Exists bool   `json:"exists"`
	Data   []byte `json:"data,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Digest string `json:"digest"`
}

func image(data []byte, exists bool, mode uint32) Image {
	// Windows reports writable regular files as 0666; POSIX creation bits are
	// not their ACL. Record the mode that filesystem inspection will return.
	if runtime.GOOS == "windows" && exists && mode&0200 != 0 {
		mode = 0666
	}
	sum := sha256.Sum256(data)
	return Image{exists, bytes.Clone(data), mode, hex.EncodeToString(sum[:])}
}
func same(a, b Image) bool {
	return a.Exists == b.Exists && a.Mode == b.Mode && a.Digest == b.Digest && bytes.Equal(a.Data, b.Data)
}
func readImage(d *storage.Dir, path string) (Image, error) {
	data, err := d.ReadFile(path, 1<<20)
	if os.IsNotExist(err) {
		return image(nil, false, 0), nil
	}
	if err != nil {
		return Image{}, err
	}
	fi, err := d.Lstat(path)
	if err != nil {
		return Image{}, err
	}
	return image(data, true, uint32(fi.Mode().Perm())), nil
}
func decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON metadata")
	}
	return nil
}
func encode(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > 1<<20 {
		return nil, errors.New("configuration transaction exceeds metadata size limit")
	}
	return b, nil
}

type Source struct {
	ID        string `json:"id"`
	Record    Image  `json:"record"`
	Selection Image  `json:"selection"`
}

func (s Source) state() (selection.State, error) {
	var state selection.State
	if err := decode(s.Selection.Data, &state); err != nil {
		return state, err
	}
	return selection.Canonical(state)
}
func (s Source) record() (workspace.Record, error) {
	var record workspace.Record
	err := decode(s.Record.Data, &record)
	return record, err
}
func selectionPath(id string) string { return filepath.Join("workspaces", id, "selection.json") }

type session struct {
	r            *gitexec.Runner
	w            *workspace.Context
	common, meta *storage.Dir
	lock         *storage.Lock
	sources      []Source
	// ignoreCase is the checkout's core.ignorecase, which shapes the
	// generated ignore patterns.
	ignoreCase bool
}

// compile returns the managed patterns for states under this checkout's
// core.ignorecase, and the other setting's patterns when they differ: blocks
// written by earlier releases, or before the setting changed, use those.
func (s *session) compile(states []selection.State) ([]byte, [][]byte, error) {
	want, err := exclude.Compile(states, s.ignoreCase)
	if err != nil {
		return nil, nil, err
	}
	other, err := exclude.Compile(states, !s.ignoreCase)
	if err != nil || bytes.Equal(other, want) {
		return want, nil, err
	}
	return want, [][]byte{other}, nil
}

func open(ctx context.Context, r *gitexec.Runner, cwd string) (*session, error) {
	return openMode(ctx, r, cwd, false)
}
func openMode(ctx context.Context, r *gitexec.Runner, cwd string, readOnly bool) (*session, error) {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return nil, err
	}
	s := &session{r: r, w: w, common: common}
	success := false
	defer func() {
		if !success {
			s.close()
		}
	}()
	// No bootstrap writes: selection commands require completed initialization.
	var repository workspace.Repository
	if err = common.ReadJSON("gitlocal/repository.json", &repository); err != nil {
		return nil, fmt.Errorf("read initialized repository (run git local init first): %w", err)
	}
	if repository.SchemaVersion != workspace.SchemaVersion || !storage.ValidUUID(repository.RepositoryID) {
		return nil, errors.New("invalid repository identity")
	}
	if readOnly {
		s.lock, err = common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	} else {
		s.lock, err = common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	}
	if err != nil {
		return nil, err
	}
	s.meta, err = storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return nil, err
	}
	var lockedRepository workspace.Repository
	if err = s.meta.ReadJSON("repository.json", &lockedRepository); err != nil {
		return nil, err
	}
	if lockedRepository != repository {
		return nil, fmt.Errorf("%w: repository changed during lock acquisition", storage.ErrConflict)
	}
	admin, err := storage.OpenDir(w.Admin)
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	var identity workspace.Identity
	if err = admin.ReadJSON("gitlocal-workspace.json", &identity); err != nil {
		return nil, fmt.Errorf("read initialized workspace (run git local init first): %w", err)
	}
	if identity.SchemaVersion != workspace.SchemaVersion || identity.RepositoryID != repository.RepositoryID || !storage.ValidUUID(identity.WorkspaceID) {
		return nil, errors.New("invalid workspace identity")
	}
	w.RepositoryID = identity.RepositoryID
	w.WorkspaceID = identity.WorkspaceID
	if s.ignoreCase, err = r.IgnoreCase(ctx, w.Root, w.Admin); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(w.Common, w.Exclude)
	if err != nil || filepath.ToSlash(rel) != "info/exclude" {
		return nil, errors.New("redirected exclusion path is unsupported")
	}
	// A migration can temporarily move history.git out of service. Surface its
	// recovery instruction before ordinary store validation reports a missing
	// directory; other transaction kinds still use their normal handlers.
	if _, err = s.pending(); err != nil {
		return nil, err
	}
	if err = gitexec.ValidateStore(w.Store); err != nil {
		return nil, err
	}
	s.sources, err = s.readSources()
	if err != nil {
		return nil, err
	}
	found := false
	for _, src := range s.sources {
		if src.ID == w.WorkspaceID {
			record, _ := src.record()
			found = record.State == "active"
		}
	}
	if !found {
		return nil, errors.New("current workspace is not an active registration")
	}
	success = true
	return s, nil
}
func (s *session) close() {
	if s.meta != nil {
		s.meta.Close()
	}
	if s.lock != nil {
		s.lock.Close()
	}
	if s.common != nil {
		s.common.Close()
	}
}
func (s *session) readSources() ([]Source, error) {
	dirs, err := s.meta.ReadDir("workspaces")
	if err != nil {
		return nil, err
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name() < dirs[j].Name() })
	sources := []Source{}
	for _, dir := range dirs {
		id := dir.Name()
		if !storage.ValidUUID(id) || !dir.IsDir() {
			return nil, errors.New("invalid workspace registration directory")
		}
		record, err := readImage(s.meta, filepath.Join("workspaces", id, "workspace.json"))
		if err != nil {
			return nil, err
		}
		selected, err := readImage(s.meta, selectionPath(id))
		if err != nil {
			return nil, err
		}
		if !record.Exists || !selected.Exists {
			return nil, errors.New("incomplete workspace registration")
		}
		src := Source{id, record, selected}
		if err = validateSource(src); err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, nil
}
func validateSource(src Source) error {
	if !storage.ValidUUID(src.ID) {
		return errors.New("invalid workspace ID")
	}
	for _, im := range []Image{src.Record, src.Selection} {
		if !im.Exists || im.Mode > 0777 || !same(im, image(im.Data, im.Exists, im.Mode)) {
			return errors.New("invalid workspace source image")
		}
	}
	record, err := src.record()
	if err != nil {
		return err
	}
	if record.SchemaVersion != workspace.SchemaVersion || record.WorkspaceID != src.ID || (record.State != "active" && record.State != "retained") {
		return errors.New("invalid workspace record")
	}
	_, err = src.state()
	return err
}
func states(sources []Source, target string, next *selection.State) ([]selection.State, error) {
	result := []selection.State{}
	for _, src := range sources {
		record, err := src.record()
		if err != nil {
			return nil, err
		}
		if record.State != "active" {
			continue
		}
		state, err := src.state()
		if err != nil {
			return nil, err
		}
		if src.ID == target && next != nil {
			state = *next
		}
		result = append(result, state)
	}
	return result, nil
}
func (s *session) pending() ([]string, error) {
	entries, err := s.meta.ReadDir("transactions")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, entry := range entries {
		if entry.Name() == "completed" && entry.IsDir() {
			continue
		}
		if storage.IsTempName(entry.Name()) && entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "encrypt-") && strings.HasSuffix(name, ".json") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, "encrypt-"), ".json")
			if storage.ValidUUID(id) && entry.Type().IsRegular() {
				return nil, fmt.Errorf("%w: unfinished encryption migration %s; run git local encrypt recover %s --resume or --rollback", storage.ErrConflict, id, id)
			}
		}
		prefix := "config-"
		if strings.HasPrefix(name, "restore-") {
			prefix = "restore-"
		} else if strings.HasPrefix(name, "seed-") {
			prefix = "seed-"
		}
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") || !storage.ValidUUID(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")) {
			return nil, fmt.Errorf("%w: unfinished or unknown transaction %q; initialization journals require git local init", storage.ErrConflict, name)
		}
		ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json"))
	}
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return nil, fmt.Errorf("%w: duplicate transaction ID", storage.ErrConflict)
		}
	}
	return ids, nil
}
