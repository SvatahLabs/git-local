//go:build darwin || linux

package restore

import (
	"io/fs"
	"os"

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
