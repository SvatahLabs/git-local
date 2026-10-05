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
