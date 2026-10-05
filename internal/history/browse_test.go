package history

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBrowseHistoricalContentWithoutChangingWorktree(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	write(t, r.Root, "notes/a", []byte("alpha\nneedle first\n"))
	write(t, r.Root, "notes/binary", []byte{0, 1, 2})
	change(t, g, r.Root, true, "notes")
	first := save(t, g, r.Root)
	write(t, r.Root, "notes/a", []byte("beta\nneedle second\n"))
	if err := os.Remove(filepath.Join(r.Root, "notes/binary")); err != nil {
		t.Fatal(err)
	}
	write(t, r.Root, "notes/new", []byte("new file\n"))
	second := save(t, g, r.Root)
	write(t, r.Root, "notes/a", []byte("unsaved current bytes\n"))
	privateBefore := durableState(t, w.Store)
	mainBefore := mainState(t, r.Root)

	old, err := Show(context.Background(), g, r.Root, "", first.ID[:10], "notes/a")
	if err != nil || !bytes.Equal(old, []byte("alpha\nneedle first\n")) {
		t.Fatalf("old show: %v %q", err, old)
	}
	latest, err := Show(context.Background(), g, r.Root, "", "latest", "notes/a")
	if err != nil || !bytes.Equal(latest, []byte("beta\nneedle second\n")) {
		t.Fatalf("latest show: %v %q", err, latest)
	}
	patch, err := DiffSnapshots(context.Background(), g, r.Root, "", first.ID, second.ID)
	if err != nil || !strings.Contains(patch, "-needle first") || !strings.Contains(patch, "+needle second") || !strings.Contains(patch, "notes/new") {
		t.Fatalf("historical diff: %v\n%s", err, patch)
	}
	entries, err := LogFiltered(context.Background(), g, r.Root, "", 20, LogFilter{WithStat: true})
	if err != nil || len(entries) != 2 || !strings.Contains(entries[0].Stat, "notes/a") || !strings.Contains(entries[0].Stat, "notes/new") || !strings.Contains(entries[1].Stat, "notes/binary") {
		t.Fatalf("historical stats: %v %+v", err, entries)
	}
	grep, err := Grep(context.Background(), g, r.Root, "", "needle", 20, 100)
	if err != nil || len(grep.Matches) != 2 || grep.Matches[0].Snapshot != second.ID || grep.Matches[1].Snapshot != first.ID || grep.SkippedBinary != 1 {
		t.Fatalf("historical grep: %v %+v", err, grep)
	}
	limited, err := Grep(context.Background(), g, r.Root, "", "needle", 20, 1)
	if err != nil || !limited.Truncated || len(limited.Matches) != 1 {
		t.Fatalf("grep limit: %v %+v", err, limited)
	}
	window, err := Grep(context.Background(), g, r.Root, "", "needle", 1, 100)
	if err != nil || len(window.Matches) != 1 || !window.MoreSnapshots || window.Searched != 1 {
		t.Fatalf("grep snapshot window: %v %+v", err, window)
	}
	if _, err := Show(context.Background(), g, r.Root, "", "HEAD", "notes/a"); err == nil {
		t.Fatal("revision expression accepted as snapshot selector")
	}
	if _, err := Show(context.Background(), g, r.Root, "", first.ID, "../app.txt"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := Show(context.Background(), g, r.Root, "", second.ID, "notes/binary"); err == nil {
		t.Fatal("deleted historical file appeared in later snapshot")
	}
	if got, _ := os.ReadFile(filepath.Join(r.Root, "notes/a")); !bytes.Equal(got, []byte("unsaved current bytes\n")) {
		t.Fatal("browsing changed the working file")
	}
	if !reflect.DeepEqual(privateBefore, durableState(t, w.Store)) || !reflect.DeepEqual(mainBefore, mainState(t, r.Root)) {
		t.Fatal("browsing changed Git state")
	}
}

func TestReadBlobEnforcesLimitBeforeContent(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	write(t, r.Root, "local", []byte("12345"))
	change(t, g, r.Root, true, "local")
	saved := save(t, g, r.Root)
	snapshot, err := Resolve(context.Background(), g, r.Root, "", saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readBlob(context.Background(), g, w.Store, snapshot.Files["local"].ID, 4); err == nil {
		t.Fatal("oversized blob was read")
	}
}
