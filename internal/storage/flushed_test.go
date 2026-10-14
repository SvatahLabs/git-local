package storage

import (
	"fmt"
	"io"
	"path/filepath"
	"testing"
)

// flushedUnder counts the flushed stages this process remembers under root.
func flushedUnder(root string) int {
	n := 0
	flushedStages.Range(func(key, _ any) bool {
		if rel, err := filepath.Rel(root, key.(string)); err == nil && filepath.IsLocal(rel) {
			n++
		}
		return true
	})
	return n
}

// A stage is forgotten once it is published or removed, so a long seed
// doesn't keep every stage's name (GL-120); a stage still waiting stays
// flushed.
func TestFlushedStagesAreForgottenOncePublished(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	write := func(out io.Writer) error { _, err := io.WriteString(out, "data"); return err }
	for i := 0; i < 3; i++ {
		stage := fmt.Sprintf(".gitlocal-tmp-%d", i)
		if err := d.StageNamed(stage, 0644, write); err != nil {
			t.Fatal(err)
		}
	}
	if n := flushedUnder(d.name); n != 3 {
		t.Fatalf("flushed stages after staging: %d", n)
	}
	if err := d.RenameNoReplace(".gitlocal-tmp-0", "published"); err != nil {
		t.Fatal(err)
	}
	if err := d.Rename(".gitlocal-tmp-1", "replaced"); err != nil {
		t.Fatal(err)
	}
	if n := flushedUnder(d.name); n != 1 {
		t.Fatalf("flushed stages after publishing two: %d", n)
	}
	if err := d.SyncStagedFile(".gitlocal-tmp-2"); err != nil {
		t.Fatal(err)
	}
	if n := flushedUnder(d.name); n != 1 {
		t.Fatalf("a waiting stage was forgotten: %d", n)
	}
	if err := d.Remove(".gitlocal-tmp-2"); err != nil {
		t.Fatal(err)
	}
	if n := flushedUnder(d.name); n != 0 {
		t.Fatalf("flushed stages after removing the last: %d", n)
	}
}
