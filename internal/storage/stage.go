package storage

import (
	"io"
	"io/fs"
	"os"
)

// StageNamed exclusively creates a journal-reserved sibling file. It never
// truncates an existing inode, including leftovers from an interrupted attempt.
func (d *Dir) StageNamed(name string, mode fs.FileMode, write func(io.Writer) error) error {
	f, err := d.open(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = write(f)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		d.flushed(name)
	}
	// Preserve partial bytes on failure. They may include an external edit;
	// only recorded complete images can be safely recognized by recovery.
	return err
}

// ChmodStaged applies a journal-reserved stage's final mode again. A process
// that died after the last write but before StageNamed's Chmod leaves complete
// bytes at the 0600 creation mode.
func (d *Dir) ChmodStaged(name string, mode fs.FileMode) error {
	return d.chmodStaged(name, mode)
}

// MakeDirectory creates exactly one private directory, never accepts an existing one.
func (d *Dir) MakeDirectory(name string) error { return d.mkdir(name) }
