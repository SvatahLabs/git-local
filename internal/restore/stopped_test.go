package restore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/storage"
)

// GL-119: a restore stopped at a preserved external edit, or at staging bytes
// it doesn't recognize, names the way out rather than only resume and
// rollback, which both refuse while the edit or the bytes are there.
func TestStoppedAtExternalEditNamesTheWayOut(t *testing.T) {
	t.Parallel()
	id := "00000000-0000-4000-8000-000000000001"
	// No journal there: nothing is recorded.
	meta, err := storage.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	for _, c := range []struct {
		err       error
		want, not []string
	}{
		{&externalEdit{path: "notes/c", aside: true}, []string{`new external edit at "notes/c"; preserved`, "move notes/c aside", "recover " + id + " --resume or --rollback", "recover " + id + " --abandon"}, nil},
		{&externalEdit{path: "notes/c"}, []string{`new external edit at "notes/c"; preserved`, "recover " + id + " --abandon", "undo the edit, then run git local recover " + id + " --resume or --rollback"}, []string{"aside"}},
		{&unknownStage{path: "notes/.gitlocal-tmp-x"}, []string{`unrecognized staging bytes at "notes/.gitlocal-tmp-x"; preserved`, "move notes/.gitlocal-tmp-x aside", "recover " + id + " --resume or --rollback", "recover " + id + " --abandon"}, nil},
	} {
		err := stopped(meta, &journal{ID: id}, c.err)
		for _, want := range c.want {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("message lacks %q: %v", want, err)
			}
		}
		for _, not := range c.not {
			if strings.Contains(err.Error(), not) {
				t.Fatalf("message has %q: %v", not, err)
			}
		}
		if !errors.Is(err, storage.ErrConflict) || !errors.Is(c.err, storage.ErrConflict) {
			t.Fatalf("not a conflict: %v", err)
		}
	}
	if other := stopped(meta, &journal{ID: id}, errors.New("disk full")); strings.Contains(other.Error(), "aside") {
		t.Fatalf("other failures name an edit: %v", other)
	}
}

// The way out works. A file the restore replaces exists before and after it,
// so the edit moved aside is a change too and resume still stops; undoing
// the edit lets it finish. A file the restore creates is absent before it, so
// moving the edit aside lets resume finish.
func TestStoppedAtExternalEditWayOutWorks(t *testing.T) {
	for _, kind := range []string{"replaced", "created"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			repo, r, _ := fixture(t)
			root := repo.Root
			write(t, root, "notes/a", "source")
			write(t, root, "notes/b", "kept")
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			if kind == "replaced" {
				write(t, root, "notes/a", "baseline")
			} else {
				remove(t, root, "notes/a")
			}
			save(t, r, root)
			result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
				if at == "before-install:0" {
					write(t, root, "notes/a", "external")
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), `new external edit at "notes/a"`) {
				t.Fatalf("restore over an external edit: %v", err)
			}
			aside := filepath.Join(t.TempDir(), "a")
			if err := os.Rename(filepath.Join(root, "notes", "a"), aside); err != nil {
				t.Fatal(err)
			}
			_, resumed := Recover(ctx, r, root, result.TransactionID, false)
			if kind == "replaced" {
				if strings.Contains(err.Error(), "aside") || resumed == nil || !strings.Contains(resumed.Error(), `new external edit at "notes/a"`) {
					t.Fatalf("advice %v; resume with the edit moved aside: %v", err, resumed)
				}
				write(t, root, "notes/a", "baseline")
				_, resumed = Recover(ctx, r, root, result.TransactionID, false)
			} else if !strings.Contains(err.Error(), "move notes/a aside") {
				t.Fatalf("advice: %v", err)
			}
			if resumed != nil {
				t.Fatalf("resume after following the advice: %v", resumed)
			}
			if b, e := os.ReadFile(filepath.Join(root, "notes", "a")); e != nil || string(b) != "source" {
				t.Fatalf("notes/a after resume: %q %v", b, e)
			}
			if b, e := os.ReadFile(aside); e != nil || string(b) != "external" {
				t.Fatalf("edit moved aside: %q %v", b, e)
			}
		})
	}
}
