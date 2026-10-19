package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gitlocal/internal/storage"
)

func durableState(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Status may fill the stat cache; it records no state (GL-138).
			if d.Name() == StatCacheDir && filepath.Base(filepath.Dir(path)) == "gitlocal" {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(path)
			if e != nil {
				return e
			}
			result[path] = target
			return nil
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		result[path] = fmt.Sprintf("%v:%v:%s", fi.Mode(), fi.ModTime().UnixNano(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestStatusAndDiffReadOnly(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	write(t, r.Root, "notes/change", []byte("before\n"))
	write(t, r.Root, "notes/delete", []byte("gone\n"))
	write(t, r.Root, "notes/deselect", []byte("keep working\n"))
	write(t, r.Root, "notes/binary", []byte("old\x00\xff"))
	change(t, g, r.Root, true, "notes")
	save(t, g, r.Root)
	write(t, r.Root, "notes/change", []byte("after\n"))
	write(t, r.Root, "notes/binary", []byte("new\x00\xfe"))
	write(t, r.Root, "notes/add", []byte("addition\n"))
	os.Remove(filepath.Join(r.Root, "notes/delete"))
	change(t, g, r.Root, false, "notes/deselect")
	before := durableState(t, r.Root)
	status, err := Status(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{{"A", "notes/add"}, {"M", "notes/binary"}, {"M", "notes/change"}, {"D", "notes/delete"}, {"D", "notes/deselect"}}
	if !reflect.DeepEqual(status.Files, want) {
		t.Fatalf("changes: %#v", status.Files)
	}
	if !reflect.DeepEqual(status.Selection, []SelectionChange{{"A", "exclude", "notes/deselect"}}) {
		t.Fatal(status.Selection)
	}
	if !reflect.DeepEqual(before, durableState(t, r.Root)) {
		t.Fatal("status persisted state/content")
	}
	var temporary string
	result, err := diff(context.Background(), g, r.Root, func(stage, path string) error { temporary = path; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Patch, "-before\n+after") || !strings.Contains(result.Patch, "Binary files") || strings.Contains(result.Patch, "new\x00") {
		t.Fatalf("unexpected diff: %q", result.Patch)
	}
	if _, err = os.Stat(filepath.Dir(temporary)); !os.IsNotExist(err) {
		t.Fatal("temporary diff store survived", err)
	}
	if !reflect.DeepEqual(before, durableState(t, r.Root)) {
		t.Fatal("diff changed durable state/content")
	}
	if _, err = os.Stat(filepath.Join(w.Store, "objects/info/alternates")); !os.IsNotExist(err) {
		t.Fatal("durable alternate created")
	}
}
func TestPorcelainUnusualPathsAndUnborn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tab and wildcard filenames not supported")
	}
	r, g, _ := fixture(t)
	name := "notes/a\tb[*]é"
	write(t, r.Root, name, []byte("private"))
	change(t, g, r.Root, true, name)
	s, err := Status(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("F\tA\t" + name + "\x00S\tA\tfile\t" + name + "\x00")
	if !bytes.Equal(s.Porcelain(), want) {
		t.Fatalf("porcelain = %q", s.Porcelain())
	}
	if strings.Contains(s.Human(), "a\tb") {
		t.Fatal("human path contains raw tab")
	}
	d, err := Diff(context.Background(), g, r.Root)
	if err != nil || !strings.Contains(d.Patch, "+private") {
		t.Fatal(d, err)
	}
}
func TestOwnershipConflictsVisibleAndNotCaptured(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, "notes/owned", []byte("old snapshot"))
	write(t, r.Root, "notes/safe", []byte("old safe"))
	change(t, g, r.Root, true, "notes")
	save(t, g, r.Root)
	secret := []byte("MAIN-OWNED-NEVER-CAPTURE\x00\xff")
	write(t, r.Root, "notes/owned", secret)
	r.Git("add", "-f", "notes/owned")
	write(t, r.Root, "notes/safe", []byte("safe edited"))
	// A hardlink would fail capture if the owned file were traversed/read.
	if err := os.Link(filepath.Join(r.Root, "notes/owned"), filepath.Join(r.Root, "owned-alias")); err != nil {
		t.Fatal(err)
	}
	before := durableState(t, w.Store)
	status, err := Status(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.Files, []FileChange{{"C", "notes/owned"}, {"M", "notes/safe"}}) {
		t.Fatal(status.Files)
	}
	result, err := Diff(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Patch, "owned") || strings.Contains(result.Patch, "MAIN-OWNED") || !strings.Contains(result.Patch, "safe edited") {
		t.Fatal("conflicted content entered patch", result.Patch)
	}
	if !reflect.DeepEqual(before, durableState(t, w.Store)) {
		t.Fatal("inspection changed history")
	}
}
func TestDiffCleansOnErrorAndCancellation(t *testing.T) {
	r, g, _ := fixture(t)
	write(t, r.Root, "local", []byte("private"))
	change(t, g, r.Root, true, "local")
	for _, stage := range []string{"diff-captured", "diff-rendered"} {
		ctx, cancel := context.WithCancel(context.Background())
		var temporary string
		_, err := diff(ctx, g, r.Root, func(at, path string) error {
			temporary = path
			if at == stage {
				cancel()
				return errors.New("injected")
			}
			return nil
		})
		cancel()
		if err == nil {
			t.Fatal("missed failure")
		}
		if _, err = os.Stat(filepath.Dir(temporary)); !os.IsNotExist(err) {
			t.Fatal("transient content survived error", temporary, err)
		}
	}
}
func TestLogOmitsRecoveryAndDoesNotDependOnWorkingFiles(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, "local", []byte("first"))
	change(t, g, r.Root, true, "local")
	first := save(t, g, r.Root)
	write(t, r.Root, "local", []byte("second"))
	second := save(t, g, r.Root)
	// An independent commit anchored only by a recovery ref must not be enumerated.
	treeID := strings.TrimSpace(string(gitHistory(t, g, w, "rev-parse", first.ID+"^{tree}")))
	recovery, err := oid(g.History(context.Background(), w.Store, strings.NewReader("recovery only"), "commit-tree", treeID))
	if err != nil {
		t.Fatal(err)
	}
	gitHistory(t, g, w, "update-ref", "refs/gitlocal/recovery/"+w.WorkspaceID+"/fixture", recovery)
	os.Remove(filepath.Join(r.Root, "local"))
	before := durableState(t, w.Store)
	entries, err := Log(context.Background(), g, r.Root, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != second.ID || entries[1].ID != first.ID {
		t.Fatal(entries)
	}
	limited, err := Log(context.Background(), g, r.Root, 1)
	if err != nil || len(limited) != 1 {
		t.Fatal(limited, err)
	}
	if strings.Contains(FormatLog(entries), "\nraw message") {
		t.Fatal("unquoted log control characters")
	}
	if !reflect.DeepEqual(before, durableState(t, w.Store)) {
		t.Fatal("log changed history")
	}
}
func TestReadCommandsDoNotRecreateMissingLock(t *testing.T) {
	r, g, w := fixture(t)
	lock := filepath.Join(w.Common, "gitlocal/locks/repository.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(context.Background(), g, r.Root); err == nil {
		t.Fatal("status recreated lock")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("status wrote state", err)
	}
}
func TestReadCommandsRefusePendingTransaction(t *testing.T) {
	r, g, w := fixture(t)
	id, _ := storage.UUID()
	write(t, w.Common, "gitlocal/transactions/config-"+id+".json", []byte("{}"))
	if _, err := Status(context.Background(), g, r.Root); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if _, err := Diff(context.Background(), g, r.Root); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if _, err := Log(context.Background(), g, r.Root, 20); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
}

func TestIgnoreConflictAndDiffControls(t *testing.T) {
	r, g, _ := fixture(t)
	write(t, r.Root, "notes/conflict", []byte("before"))
	write(t, r.Root, "notes/safe", []byte("old\n"))
	change(t, g, r.Root, true, "notes")
	change(t, g, r.Root, false, "notes/corridor")
	save(t, g, r.Root)
	// A corridor leaves the directory traversable, so a higher-priority negation applies.
	write(t, r.Root, ".gitignore", []byte("!notes/conflict\n"))
	write(t, r.Root, "notes/conflict", []byte("must-not-appear-in-patch"))
	write(t, r.Root, "notes/safe", []byte("\x1b[31mchanged\n"))
	write(t, r.Root, ".gitattributes", []byte("* diff=poison\n"))
	r.Git("config", "diff.external", "exit 91")
	r.Git("config", "diff.poison.command", "exit 92")
	r.Git("config", "diff.poison.textconv", "exit 93")
	s, err := Status(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 2 || s.Files[0].Code != "C" || len(s.Warnings) == 0 {
		t.Fatal(s)
	}
	d, err := Diff(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Patch, "must-not-appear") || strings.ContainsRune(d.Patch, '\x1b') || !strings.Contains(d.Patch, "\\u001b") {
		t.Fatalf("unsafe patch: %q", d.Patch)
	}
	write(t, r.Root, "notes/safe", []byte("non-utf8-\xff"))
	d, err = Diff(context.Background(), g, r.Root)
	if err != nil || !strings.Contains(d.Patch, "Non-UTF-8 content detected") {
		t.Fatal(d, err)
	}
}
