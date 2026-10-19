//go:build darwin || linux

package history

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

const statKeysSupported = true

// statKeyOf reads the stat cache key from storage.Dir's Lstat, or from the
// fstat of an open handle (storage.Observation.Info).
func statKeyOf(info fs.FileInfo) (statKey, bool) {
	if info == nil {
		return statKey{}, false
	}
	switch st := info.Sys().(type) {
	case *unix.Stat_t:
		return statKey{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: st.Size, Mtime: unix.TimespecToNsec(st.Mtim), Ctime: unix.TimespecToNsec(st.Ctim), Mode: uint32(st.Mode)}, true
	case *syscall.Stat_t:
		return handleStatKey(st), true
	}
	return statKey{}, false
}
