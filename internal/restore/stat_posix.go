//go:build darwin || linux

package restore

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// sameFile compares file identity and change time. storage.Dir's Lstat
// returns its own FileInfo, which os.SameFile doesn't recognize.
func sameFile(a, b fs.FileInfo) bool {
	sa, okA := a.Sys().(*unix.Stat_t)
	sb, okB := b.Sys().(*unix.Stat_t)
	if !okA || !okB {
		return os.SameFile(a, b)
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino && sa.Ctim == sb.Ctim
}

// changeTime returns the inode change time, or zero when it is unknown.
func changeTime(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*unix.Stat_t); ok {
		return time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
	}
	return time.Time{}
}

// indexStat stats the index file the way Git replaces it: following a
// symlink (Git locks and replaces the link's target), with a FileInfo whose
// Sys is a *unix.Stat_t, so sameFile and racy compare its change time too.
func indexStat(path string) (fs.FileInfo, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return unixInfo{filepath.Base(path), st}, nil
}

type unixInfo struct {
	name string
	st   unix.Stat_t
}

func (i unixInfo) Name() string       { return i.name }
func (i unixInfo) Size() int64        { return i.st.Size }
func (i unixInfo) Mode() fs.FileMode  { return fs.FileMode(i.st.Mode & 0777) }
func (i unixInfo) ModTime() time.Time { return time.Unix(int64(i.st.Mtim.Sec), int64(i.st.Mtim.Nsec)) }
func (i unixInfo) IsDir() bool        { return i.st.Mode&unix.S_IFMT == unix.S_IFDIR }
func (i unixInfo) Sys() any           { return &i.st }
