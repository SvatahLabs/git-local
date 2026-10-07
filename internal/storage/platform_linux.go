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

// renameNoReplace refuses an existing destination atomically. On a
// filesystem without RENAME_NOREPLACE, a hard link, which fails when the
// destination exists, followed by removing the old name does the same.
func renameNoReplace(fromfd int, from string, tofd int, to string) error {
	err := unix.Renameat2(fromfd, from, tofd, to, unix.RENAME_NOREPLACE)
	if err != unix.EINVAL && err != unix.ENOSYS {
		return err
	}
	if err := unix.Linkat(fromfd, from, tofd, to, 0); err != nil {
		return err
	}
	return unix.Unlinkat(fromfd, from, 0)
}
