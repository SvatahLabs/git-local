//go:build !darwin && !linux

package selection

import (
	"io/fs"
	"os"
	"time"
)

// SameFile compares file identity; when it can't be established, the image
// is hashed again.
func SameFile(a, b fs.FileInfo) bool { return os.SameFile(a, b) }

// ChangeTime is unknown here; Racy uses the modification time alone.
func ChangeTime(fs.FileInfo) time.Time { return time.Time{} }

// StatIndex stats the index file, following a symlink as Git does.
func StatIndex(path string) (fs.FileInfo, error) { return os.Stat(path) }

// lstatRegular stats a file without following a symlink; it reports whether
// the file is a regular file.
func lstatRegular(path string) (fs.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	return info, info.Mode().IsRegular(), nil
}
