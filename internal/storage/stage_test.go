package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStageNeverTruncatesExistingAndPreservesFailedWrites(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = os.WriteFile(filepath.Join(root, "existing"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = d.StageNamed("existing", 0600, func(w io.Writer) error { _, e := w.Write([]byte("overwrite")); return e }); err == nil {
		t.Fatal("stage overwrote existing inode")
	}
	diskFull := errors.New("simulated disk full")
	if err = d.StageNamed("partial", 0600, func(w io.Writer) error {
		if _, e := w.Write([]byte("partial")); e != nil {
			return e
		}
		return diskFull
	}); !errors.Is(err, diskFull) {
		t.Fatal(err)
	}
	partial, err := d.ReadFile("partial", 100)
	if err != nil || string(partial) != "partial" {
		t.Fatal("partial bytes were lost", err)
	}
	b, err := d.ReadFile("existing", 100)
	if err != nil || string(b) != "keep" {
		t.Fatal(string(b), err)
	}
}
