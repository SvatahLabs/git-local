//go:build !darwin && !linux

package restore

import (
	"io/fs"
	"os"
	"time"
)

// sameFile compares file identity; when it can't be established, the image
// is hashed again.
func sameFile(a, b fs.FileInfo) bool { return os.SameFile(a, b) }

// changeTime is unknown here; racy uses the modification time alone.
func changeTime(fs.FileInfo) time.Time { return time.Time{} }

// indexStat stats the index file, following a symlink as Git does.
func indexStat(path string) (fs.FileInfo, error) { return os.Stat(path) }
