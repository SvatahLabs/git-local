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
