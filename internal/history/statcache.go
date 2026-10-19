package history

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"sync/atomic"
	"time"

	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// StatCacheDir holds, in the private state directory, one stat cache per
// workspace: each selected path's stat fields and the blob ID of the content
// they were hashed from, so status and snapshots don't read unchanged files
// again (GL-138). It is only a cache: backups leave it out, and an entry is
// trusted only when every field still matches.
//
// The cache is in the same trust domain as the history store: whoever can
// write it can rewrite history as well.
const StatCacheDir = "statcache"

const (
	statCacheVersion = 1
	// statCacheLimit bounds the file read back; a larger one is ignored, and
	// save doesn't write one.
	statCacheLimit = 256 << 20
)

// statCacheLimitBytes is statCacheLimit, variable for tests.
var statCacheLimitBytes int64 = statCacheLimit

// statCacheBeforeObserve, when set, runs in lookup between the Lstat and the
// open, so tests can replace the file there.
var statCacheBeforeObserve func(path string)

// statKey is what a file's Lstat must still show for its cached blob ID to
// be trusted.
type statKey struct {
	Dev   uint64 `json:"dev"`
	Ino   uint64 `json:"ino"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Ctime int64  `json:"ctime"`
	Mode  uint32 `json:"mode"`
}

type statEntry struct {
	Key statKey `json:"key"`
	ID  string  `json:"id"`
}

type statCacheFile struct {
	Version int                  `json:"version"`
	Entries map[string]statEntry `json:"entries"`
}

// filesHashed counts selected files whose content status or a snapshot read
// to hash; tests use it to prove a warm run reads none.
var filesHashed atomic.Int64

// statCacheNow is the clock a stat cache compares timestamps with.
var statCacheNow = time.Now

// statCache is one workspace's stat cache. A nil *statCache caches nothing:
// on platforms without inode numbers and change times, and for paths the
// sensitive-file policy covers, whose plaintext blob IDs must not be kept
// unencrypted.
type statCache struct {
	state     *storage.Dir
	name      string
	old, next map[string]statEntry
	cacheable func(string) bool
	// dirty records that load dropped entries the file still holds.
	dirty bool
}

// statProbe is a file's Lstat and the time, taken before its content is
// read, that store needs to decide whether the result can be cached.
type statProbe struct {
	info   fs.FileInfo
	hashed time.Time
}

// loadStatCache opens w's stat cache; any problem reading it leaves it empty.
// cacheable reports which paths may be cached: entries for others (a path
// that has since become sensitive) are dropped, and the file is rewritten
// without them.
func loadStatCache(w workspace.Context, cacheable func(string) bool) *statCache {
	if !statKeysSupported {
		return nil
	}
	state, err := storage.OpenDir(filepath.Dir(w.Store))
	if err != nil {
		return nil
	}
	c := &statCache{state: state, name: filepath.Join(StatCacheDir, w.WorkspaceID+".json"), old: map[string]statEntry{}, next: map[string]statEntry{}, cacheable: cacheable}
	var file statCacheFile
	if data, err := state.ReadFile(c.name, statCacheLimitBytes); err == nil && json.Unmarshal(data, &file) == nil && file.Version == statCacheVersion && file.Entries != nil {
		for path, entry := range file.Entries {
			if cacheable(path) {
				c.old[path] = entry
			} else {
				c.dirty = true
			}
		}
	}
	return c
}

// lookup returns path's cached blob ID when its Lstat and the fstat of the
// opened file both still match the entry, with that observation for a later
// recheck; no content is read. Otherwise it returns the probe for store.
func (c *statCache) lookup(files *storage.Dir, path string, mode fs.FileMode) (string, storage.Observation, statProbe, bool) {
	if c == nil || !c.cacheable(path) {
		return "", storage.Observation{}, statProbe{}, false
	}
	hashed := statCacheNow()
	info, err := files.Lstat(path)
	if err != nil {
		return "", storage.Observation{}, statProbe{}, false
	}
	probe := statProbe{info, hashed}
	key, ok := statKeyOf(info)
	entry, cached := c.old[path]
	if !ok || !cached || entry.Key != key || info.Mode() != mode {
		return "", storage.Observation{}, probe, false
	}
	if statCacheBeforeObserve != nil {
		statCacheBeforeObserve(path)
	}
	// The file opened must be the one the entry describes, not another
	// renamed over the path since the Lstat with the same size and mtime.
	observation, err := files.Observe(path)
	if err != nil || observation.Mode() != mode {
		return "", storage.Observation{}, probe, false
	}
	if opened, ok := statKeyOf(observation.Info()); !ok || opened != entry.Key {
		return "", storage.Observation{}, probe, false
	}
	c.next[path] = entry
	return entry.ID, observation, statProbe{}, true
}

// store records id, hashed from path's content after probe, when the handle
// that was read (hashed, from its fstat) shows the probe's Lstat and the
// timestamps aren't racy: a same-size rewrite within their tick would leave
// every field equal. The read itself checks that the handle didn't change
// and that the path still names it.
func (c *statCache) store(path string, probe statProbe, hashed storage.Observation, id string) {
	if c == nil || probe.info == nil || selection.Racy(probe.info, probe.hashed) {
		return
	}
	key, ok := statKeyOf(probe.info)
	read, okRead := statKeyOf(hashed.Info())
	if ok && okRead && key == read {
		c.next[path] = statEntry{Key: key, ID: id}
	}
}

// retain keeps the read entries of the paths keep reports, which this run
// doesn't inspect (a scoped snapshot), when they may still be cached; keep
// must leave out paths no longer selected.
func (c *statCache) retain(keep func(string) bool) {
	if c == nil {
		return
	}
	for path, entry := range c.old {
		if _, ok := c.next[path]; !ok && keep(path) && c.cacheable(path) {
			c.next[path] = entry
		}
	}
}

// save writes the entries this run used or added, when they differ from
// those it read; paths no longer selected drop out. A cache over the load
// limit isn't written, and the old file is removed, so later runs don't
// read it only to ignore it. A failure only costs the next run its reads.
func (c *statCache) save() {
	if c == nil || (!c.dirty && maps.Equal(c.old, c.next)) {
		return
	}
	data, err := json.Marshal(statCacheFile{Version: statCacheVersion, Entries: c.next})
	if err != nil {
		return
	}
	if int64(len(data)) > statCacheLimitBytes {
		_ = c.state.Remove(c.name)
		return
	}
	if c.state.MkdirAll(StatCacheDir) != nil {
		return
	}
	_ = c.state.AtomicWrite(c.name, data, 0600)
}

// DropStatCaches removes every workspace's stat cache from state, the
// private state directory (.git/gitlocal), so no plaintext blob ID of a path
// that has just become sensitive survives a policy change. The caller holds
// the exclusive lock.
func DropStatCaches(state *storage.Dir) error {
	entries, err := state.ReadDir(StatCacheDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := state.Remove(filepath.Join(StatCacheDir, entry.Name())); err != nil {
			return err
		}
	}
	if err := state.RemoveDirectory(StatCacheDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (c *statCache) close() {
	if c != nil {
		c.state.Close()
	}
}
