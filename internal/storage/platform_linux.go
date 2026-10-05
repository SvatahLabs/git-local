package storage

import (
	"fmt"
	"golang.org/x/sys/unix"
	"time"
)

func (s statInfo) ModTime() time.Time { return time.Unix(s.st.Mtim.Sec, s.st.Mtim.Nsec) }
func localFilesystem(fd int) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return err
	}
	switch st.Type {
	case unix.EXT4_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.TMPFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
		return nil
	}
	return fmt.Errorf("unsupported filesystem type %#x", st.Type)
}
