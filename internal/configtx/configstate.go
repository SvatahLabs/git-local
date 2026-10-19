package configtx

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
)

// configState is what recognize reads, by stat: the workspace registrations
// directory, each recorded workspace's record and selection, and
// info/exclude. A seed holds the exclusive lock, so only an edit outside
// git-local changes them; reading this before each seeded file and
// recognizing again only when it changed keeps the seed's configuration work
// from growing with its files times the configuration's size (GL-122).
type configState struct {
	files []fs.FileInfo // nil when absent
	at    time.Time
}

// configNow is when a configState is read; a test moves it past the racy
// margin of the seed's own configuration writes.
var configNow = time.Now

func (s *session) readConfigState(j Journal) (configState, error) {
	state := configState{at: configNow()}
	add := func(d *storage.Dir, name string) error {
		info, err := d.Lstat(name)
		if os.IsNotExist(err) {
			info, err = nil, nil
		}
		state.files = append(state.files, info)
		return err
	}
	// Adding or removing a registration changes the directory.
	if err := add(s.meta, "workspaces"); err != nil {
		return state, err
	}
	for _, src := range j.Sources {
		if err := add(s.meta, filepath.Join("workspaces", src.ID, "workspace.json")); err != nil {
			return state, err
		}
		if err := add(s.meta, selectionPath(src.ID)); err != nil {
			return state, err
		}
	}
	return state, add(s.common, "info/exclude")
}

// same reports whether no file changed between two readings.
func (c configState) same(o configState) bool {
	if len(c.files) != len(o.files) {
		return false
	}
	for i := range c.files {
		a, b := c.files[i], o.files[i]
		if a == nil || b == nil {
			if a != b {
				return false
			}
		} else if !selection.SameIndexFile(a, b) {
			return false
		}
	}
	return true
}

// settled reports whether a later reading would show a change (see
// selection.Racy).
func (c configState) settled() bool {
	for _, info := range c.files {
		if info != nil && selection.Racy(info, c.at) {
			return false
		}
	}
	return true
}
