package storage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type Dir struct {
	root    *os.Root
	name    string
	handles []windows.Handle
}

func openHandle(path string, access, creation uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, creation, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}
func inspectHandle(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse points are unsupported")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks != 1 {
		return errors.New("hard-linked files are unsupported")
	}
	return nil
}
func OpenDir(path string) (*Dir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs) + `\`
	p, err := windows.UTF16PtrFromString(volume)
	if err != nil {
		return nil, err
	}
	if windows.GetDriveType(p) != windows.DRIVE_FIXED {
		return nil, errors.New("only local fixed Windows volumes are supported")
	}
	var handles []windows.Handle
	closeAll := func() {
		for i := len(handles) - 1; i >= 0; i-- {
			windows.CloseHandle(handles[i])
		}
	}
	current := volume
	rel, err := filepath.Rel(volume, abs)
	if err != nil {
		return nil, err
	}
	components := strings.Split(rel, string(filepath.Separator))
	if rel == "." {
		components = nil
	}
	for _, part := range append([]string{""}, components...) {
		if part != "" {
			current = filepath.Join(current, part)
		}
		h, e := openHandle(current, windows.GENERIC_READ, windows.OPEN_EXISTING)
		if e != nil {
			closeAll()
			return nil, e
		}
		handles = append(handles, h)
		if e = inspectHandle(h); e != nil {
			closeAll()
			return nil, e
		}
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		closeAll()
		return nil, err
	}
	return &Dir{root, abs, handles}, nil
}
func (d *Dir) Close() error {
	err := d.root.Close()
	for i := len(d.handles) - 1; i >= 0; i-- {
		e := windows.CloseHandle(d.handles[i])
		if err == nil {
			err = e
		}
	}
	return err
}

// Parent handles deny deletion/rename while an operation runs. Each component
// is opened with OPEN_REPARSE_POINT, so even in-root junctions are rejected.
func (d *Dir) guard(name string) (func(), error) {
	if err := validRelative(name); err != nil {
		return nil, err
	}
	var handles []windows.Handle
	closeAll := func() {
		for i := len(handles) - 1; i >= 0; i-- {
			windows.CloseHandle(handles[i])
		}
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	path := d.name
	for _, part := range parts[:len(parts)-1] {
		path = filepath.Join(path, part)
		h, err := openHandle(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
		if err != nil {
			closeAll()
			return nil, err
		}
		handles = append(handles, h)
		if err = inspectHandle(h); err != nil {
			closeAll()
			return nil, err
		}
	}
	return closeAll, nil
}
func (d *Dir) open(name string, flags int, mode fs.FileMode) (*os.File, error) {
	done, err := d.guard(name)
	if err != nil {
		return nil, err
	}
	defer done()
	creation := uint32(windows.OPEN_EXISTING)
	if flags&os.O_CREATE != 0 {
		creation = windows.OPEN_ALWAYS
	}
	if flags&os.O_EXCL != 0 {
		creation = windows.CREATE_NEW
	}
	access := uint32(windows.GENERIC_READ)
	if flags&(os.O_WRONLY|os.O_RDWR) != 0 {
		access |= windows.GENERIC_WRITE
	}
	h, err := openHandle(filepath.Join(d.name, name), access, creation)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	if err = inspectHandle(h); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}
func ValidateInfo(fi fs.FileInfo) error {
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errors.New("reparse or irregular files are unsupported")
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return errors.New("special files are unsupported")
	}
	return nil
}
func (d *Dir) Lstat(name string) (fs.FileInfo, error) {
	done, err := d.guard(name)
	if err != nil {
		return nil, err
	}
	defer done()
	return d.root.Lstat(name)
}
func (d *Dir) mkdir(name string) error {
	done, err := d.guard(name)
	if err != nil {
		return err
	}
	defer done()
	return d.root.Mkdir(name, 0700)
}
func (d *Dir) Rename(old, new string) error {
	a, err := d.guard(old)
	if err != nil {
		return err
	}
	defer a()
	b, err := d.guard(new)
	if err != nil {
		return err
	}
	defer b()
	src, err := windows.UTF16PtrFromString(filepath.Join(d.name, old))
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(filepath.Join(d.name, new))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
func (d *Dir) Remove(name string) error {
	done, err := d.guard(name)
	if err != nil {
		return err
	}
	defer done()
	return d.root.Remove(name)
}
func tryLock(f *os.File, shared bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
	if err == windows.ERROR_LOCK_VIOLATION {
		return false, nil
	}
	return err == nil, err
}

func (d *Dir) ReadDir(name string) ([]os.DirEntry, error) {
	if name == "" {
		f, err := d.root.Open(".")
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return f.ReadDir(-1)
	}
	done, err := d.guard(name)
	if err != nil {
		return nil, err
	}
	defer done()
	f, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}
func (d *Dir) ReadLink(name string) (string, error) {
	done, err := d.guard(name)
	if err != nil {
		return "", err
	}
	defer done()
	h, err := openHandle(filepath.Join(d.name, name), windows.GENERIC_READ, windows.OPEN_EXISTING)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var info struct{ Attributes, Tag uint32 }
	if err = windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return "", err
	}
	if info.Tag != windows.IO_REPARSE_TAG_SYMLINK {
		return "", errors.New("only symbolic links may be captured; junctions and other reparse points are unsupported")
	}
	return d.root.Readlink(name)
}

func (d *Dir) RemoveDirectory(name string) error {
	fi, err := d.Lstat(name)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("expected directory")
	}
	return d.Remove(name)
}

func (d *Dir) StageNamedSymlink(name, target string) error {
	return errors.New("symlink restore is unsupported on Windows")
}

func (d *Dir) SyncStaged(name string) error {
	f, err := d.open(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
