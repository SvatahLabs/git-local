package history

import (
	"encoding/json"
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
const StatCacheDir = "statcache"

const (
	statCacheVersion = 1
	// statCacheLimit bounds the file read back; a larger one is ignored.
	statCacheLimit = 256 << 20
)

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
}

// statProbe is a file's Lstat and the time, taken before its content is
// read, that store needs to decide whether the result can be cached.
type statProbe struct {
	info   fs.FileInfo
	hashed time.Time
}

// loadStatCache opens w's stat cache; any problem reading it leaves it empty.
// cacheable reports which paths may be cached.
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
	if data, err := state.ReadFile(c.name, statCacheLimit); err == nil && json.Unmarshal(data, &file) == nil && file.Version == statCacheVersion && file.Entries != nil {
		c.old = file.Entries
	}
	return c
}

// lookup returns path's cached blob ID when its Lstat still matches the
// entry, with an observation of the open file for a later recheck; no
// content is read. Otherwise it returns the probe for store.
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
	observation, err := files.Observe(path)
	if err != nil || observation.Mode() != mode || observation.Size() != info.Size() || !observation.ModTime().Equal(info.ModTime()) {
		return "", storage.Observation{}, probe, false
	}
	c.next[path] = entry
	return entry.ID, observation, statProbe{}, true
}

// store records id, hashed from path's content after probe, when the file
// still shows the probe's Lstat and its timestamps aren't racy: a same-size
// rewrite within their tick would leave every field equal.
func (c *statCache) store(files *storage.Dir, path string, probe statProbe, id string) {
	if c == nil || probe.info == nil || selection.Racy(probe.info, probe.hashed) {
		return
	}
	after, err := files.Lstat(path)
	if err != nil {
		return
	}
	key, ok := statKeyOf(probe.info)
	again, okAgain := statKeyOf(after)
	if ok && okAgain && key == again {
		c.next[path] = statEntry{Key: key, ID: id}
	}
}

// retain keeps the read entries of the paths keep reports, which this run
// doesn't inspect (a scoped snapshot).
func (c *statCache) retain(keep func(string) bool) {
	if c == nil {
		return
	}
	for path, entry := range c.old {
		if _, ok := c.next[path]; !ok && keep(path) {
			c.next[path] = entry
		}
	}
}

// save writes the entries this run used or added, when they differ from
// those it read; paths no longer selected drop out. A failure only costs
// the next run its reads.
func (c *statCache) save() {
	if c == nil || maps.Equal(c.old, c.next) {
		return
	}
	data, err := json.Marshal(statCacheFile{Version: statCacheVersion, Entries: c.next})
	if err != nil || c.state.MkdirAll(StatCacheDir) != nil {
		return
	}
	_ = c.state.AtomicWrite(c.name, data, 0600)
}

func (c *statCache) close() {
	if c != nil {
		c.state.Close()
	}
}
