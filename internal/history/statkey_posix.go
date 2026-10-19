//go:build darwin || linux

package history

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

const statKeysSupported = true

// statKeyOf reads the stat cache key from storage.Dir's Lstat.
func statKeyOf(info fs.FileInfo) (statKey, bool) {
	st, ok := info.Sys().(*unix.Stat_t)
	if !ok {
		return statKey{}, false
	}
	return statKey{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: st.Size, Mtime: unix.TimespecToNsec(st.Mtim), Ctime: unix.TimespecToNsec(st.Ctim), Mode: uint32(st.Mode)}, true
}
