//go:build darwin || linux

package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type Dir struct {
	fd   int
	name string
}

func OpenDir(path string) (*Dir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Resolve only the standard macOS /tmp and /var root aliases. Every other
	// ancestor is opened with O_NOFOLLOW, including extension state ancestors.
	for _, alias := range []string{"/tmp", "/var"} {
		if abs == alias || strings.HasPrefix(abs, alias+"/") {
			if dest, e := os.Readlink(alias); e == nil && (dest == "private"+alias || dest == "/private"+alias) {
				abs = "/private" + abs
			}
			break
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(abs, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, &os.PathError{Op: "open directory", Path: path, Err: e}
		}
		fd = next
	}
	if err = localFilesystem(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &Dir{fd, abs}, nil
}
func (d *Dir) Close() error { return unix.Close(d.fd) }
func (d *Dir) parent(name string) (int, string, error) {
	if err := validRelative(name); err != nil {
		return -1, "", err
	}
	fd, err := unix.Dup(d.fd)
	if err != nil {
		return -1, "", err
	}
	unix.CloseOnExec(fd)
	parts := strings.Split(filepath.ToSlash(name), "/")
	for _, part := range parts[:len(parts)-1] {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, "", &os.PathError{Op: "open parent", Path: name, Err: e}
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}
func (d *Dir) open(name string, flags int, mode fs.FileMode) (*os.File, error) {
	fd, base, err := d.parent(name)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	n, err := unix.Openat(fd, base, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(n), name)
	fi, err := f.Stat()
	if err == nil {
		err = ValidateInfo(fi)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func (d *Dir) Lstat(name string) (fs.FileInfo, error) {
	fd, base, err := d.parent(name)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	// os.Root gives portable FileInfo while the parent fd prevents pathname races.
	// On POSIX use fstatat for validation and an O_NOFOLLOW open for regular files.
	var st unix.Stat_t
	if err = unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &os.PathError{Op: "lstat", Path: name, Err: err}
	}
	return statInfo{base, st}, nil
}

type statInfo struct {
	name string
	st   unix.Stat_t
}

func (s statInfo) Name() string { return s.name }
func (s statInfo) Size() int64  { return s.st.Size }
func (s statInfo) IsDir() bool  { return s.Mode().IsDir() }
func (s statInfo) Sys() any     { return &s.st }
func (s statInfo) Mode() fs.FileMode {
	m := fs.FileMode(s.st.Mode & 0777)
	switch s.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= os.ModeDir
	case unix.S_IFLNK:
		m |= os.ModeSymlink
	case unix.S_IFIFO:
		m |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		m |= os.ModeSocket
	case unix.S_IFCHR:
		m |= os.ModeDevice | os.ModeCharDevice
	case unix.S_IFBLK:
		m |= os.ModeDevice
	}
	return m
}
func ValidateInfo(fi fs.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("symlinked filesystem state is unsupported")
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return errors.New("special filesystem files are unsupported")
	}
	var links uint64
	switch st := fi.Sys().(type) {
	case *syscall.Stat_t:
		links = uint64(st.Nlink)
	case *unix.Stat_t:
		links = uint64(st.Nlink)
	default:
		return errors.New("missing filesystem identity")
	}
	if fi.Mode().IsRegular() && links != 1 {
		return errors.New("hard-linked files are unsupported")
	}
	return nil
}
func (d *Dir) mkdir(name string) error {
	fd, base, err := d.parent(name)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Mkdirat(fd, base, 0700); err != nil {
		return &os.PathError{Op: "mkdir", Path: name, Err: err}
	}
	return syncDir(fd)
}
func (d *Dir) Rename(old, new string) error {
	a, base, err := d.parent(old)
	if err != nil {
		return err
	}
	defer unix.Close(a)
	b, target, err := d.parent(new)
	if err != nil {
		return err
	}
	defer unix.Close(b)
	if err = unix.Renameat(a, base, b, target); err != nil {
		return err
	}
	if err = syncDir(b); err != nil {
		return err
	}
	return syncDir(a)
}

// flushedStages holds the stages StageNamed flushed in this process: their
// bytes and mode are durable already, so SyncStaged only syncs their
// directory. A stage a dead process left is synced in full.
var flushedStages sync.Map

func (d *Dir) flushed(name string) { flushedStages.Store(filepath.Join(d.name, name), true) }

func (d *Dir) chmodStaged(name string, mode fs.FileMode) error {
	f, err := d.open(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	// A changed mode isn't durable until SyncStaged flushes the file again.
	if info, e := f.Stat(); e != nil || info.Mode().Perm() != mode.Perm() {
		flushedStages.Delete(filepath.Join(d.name, name))
	}
	err = f.Chmod(mode)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// RenameNoReplace renames like Rename, but fails with an error matching
// fs.ErrExist, changing nothing, if new already exists.
func (d *Dir) RenameNoReplace(old, new string) error {
	a, base, err := d.parent(old)
	if err != nil {
		return err
	}
	defer unix.Close(a)
	b, target, err := d.parent(new)
	if err != nil {
		return err
	}
	defer unix.Close(b)
	if err = renameNoReplace(a, base, b, target); err != nil {
		return &os.LinkError{Op: "rename", Old: old, New: new, Err: err}
	}
	if err = syncDir(b); err != nil {
		return err
	}
	// A rename within one directory (a stage published over its sibling)
	// needs that directory synced once.
	var sa, sb unix.Stat_t
	if unix.Fstat(a, &sa) == nil && unix.Fstat(b, &sb) == nil && sa.Dev == sb.Dev && sa.Ino == sb.Ino {
		return nil
	}
	return syncDir(a)
}
func (d *Dir) Remove(name string) error {
	fd, base, err := d.parent(name)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Unlinkat(fd, base, 0); err != nil {
		return &os.PathError{Op: "remove", Path: name, Err: err}
	}
	return syncDir(fd)
}
func tryLock(f *os.File, shared bool) (bool, error) {
	how := unix.LOCK_EX
	if shared {
		how = unix.LOCK_SH
	}
	err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock: %w", err)
	}
	return true, nil
}

// ReadDir reads through a pinned no-follow directory descriptor. An empty name
// denotes this root; other names must be canonical relative paths.
func (d *Dir) ReadDir(name string) ([]os.DirEntry, error) {
	parent, base := d.fd, "."
	owned := false
	if name != "" {
		var err error
		parent, base, err = d.parent(name)
		if err != nil {
			return nil, err
		}
		owned = true
	}
	if owned {
		defer unix.Close(parent)
	}
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: err}
	}
	if err = localFilesystem(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return f.ReadDir(-1)
}

// ReadLink returns the link text, never target contents, without following any
// ancestor. Unlike metadata reads this deliberately accepts a final symlink.
func (d *Dir) ReadLink(name string) (string, error) {
	parent, base, err := d.parent(name)
	if err != nil {
		return "", err
	}
	defer unix.Close(parent)
	for size := 256; size <= 1<<20; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(parent, base, buf)
		if err != nil {
			return "", &os.PathError{Op: "readlink", Path: name, Err: err}
		}
		if n < len(buf) {
			return string(buf[:n]), nil
		}
	}
	return "", errors.New("symlink text exceeds safety limit")
}

// StageNamedSymlink creates a journal-reserved link without following its target.
func (d *Dir) StageNamedSymlink(name, target string) error {
	fd, base, err := d.parent(name)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Symlinkat(target, fd, base); err != nil {
		return err
	}
	return syncDir(fd)
}

func (d *Dir) RemoveDirectory(name string) error {
	fd, base, err := d.parent(name)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Unlinkat(fd, base, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return syncDir(fd)
}

// SyncStaged re-establishes durability after a process died before closing or
// syncing a complete staging file. Symlink data is flushed through its parent.
func (d *Dir) SyncStaged(name string) error {
	fi, err := d.Lstat(name)
	if err != nil {
		return err
	}
	_, flushed := flushedStages.Load(filepath.Join(d.name, name))
	if fi.Mode()&os.ModeSymlink == 0 && !flushed {
		f, e := d.open(name, os.O_RDONLY, 0)
		if e != nil {
			return e
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	fd, _, err := d.parent(name)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return syncDir(fd)
}

// holds reports whether the open file f is still the file at name.
func (d *Dir) holds(name string, f *os.File) (bool, error) {
	var held unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &held); err != nil {
		return false, err
	}
	info, err := d.Lstat(name)
	if err != nil {
		return false, err
	}
	st := info.(statInfo).st
	return st.Dev == held.Dev && st.Ino == held.Ino, nil
}
