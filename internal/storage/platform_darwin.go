package storage

import (
	"errors"
	"golang.org/x/sys/unix"
	"time"
)

func (s statInfo) ModTime() time.Time { return time.Unix(s.st.Mtim.Sec, s.st.Mtim.Nsec) }
func localFilesystem(fd int) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return err
	}
	if st.Flags&unix.MNT_LOCAL == 0 {
		return errors.New("network filesystems are unsupported")
	}
	return nil
}

// renameNoReplace refuses an existing destination atomically.
func renameNoReplace(fromfd int, from string, tofd int, to string) error {
	return unix.RenameatxNp(fromfd, from, tofd, to, unix.RENAME_EXCL)
}

// fullSync asks the drive itself to flush, as os.File.Sync does for file
// data on Darwin; a plain fsync leaves directory entries in its cache. Tests
// replace it to observe the call.
var fullSync = func(fd int) error {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0)
	return err
}

// syncDir makes a directory's entries durable. A filesystem that can't do a
// full flush gets a plain fsync, as os.File.Sync falls back.
func syncDir(fd int) error {
	err := fullSync(fd)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
		return unix.Fsync(fd)
	}
	return err
}
