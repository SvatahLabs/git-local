package history

import "syscall"

// handleStatKey is statKeyOf for an os.File's stat, with the same values as
// for the Lstat of the same file.
func handleStatKey(st *syscall.Stat_t) statKey {
	return statKey{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: st.Size, Mtime: st.Mtimespec.Nano(), Ctime: st.Ctimespec.Nano(), Mode: uint32(st.Mode)}
}
