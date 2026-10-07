package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
)

func TestInitChecksOtherWorktreesBeforeWritingItsJournal(t *testing.T) {
	r := testutil.New(t)
	r.Commit()
	g := runner(t, r)
	ctx := context.Background()
	w, err := Init(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "broken")
	r.Git("worktree", "add", "-q", "-b", "broken", broken)
	// It holds a workspace, so init can't skip it (GL-084).
	if _, err := Init(ctx, g, broken); err != nil {
		t.Fatal(err)
	}
	gitfile, err := os.ReadFile(filepath.Join(broken, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	// Still registered and present, but no longer a checkout.
	if err := os.Remove(filepath.Join(broken, ".git")); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	r.Git("worktree", "add", "-q", "-b", "fresh", fresh)
	if _, err := Init(ctx, g, fresh); err == nil {
		t.Fatal("init accepted an unreadable registered worktree")
	}
	if _, err := os.Lstat(filepath.Join(w.Common, "gitlocal", "transactions", "init.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed init left its journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".git"), gitfile, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, g, fresh); err != nil {
		t.Fatalf("init after repair: %v", err)
	}
}

func TestAbandonInitOfRemovedWorktree(t *testing.T) {
	for _, tc := range []struct {
		name, stop    string
		newRepository bool
	}{{"new repository", "workspace", true}, {"existing repository", "selection", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := testutil.New(t)
			r.Commit()
			g := runner(t, r)
			ctx := context.Background()
			if !tc.newRepository {
				if _, err := Init(ctx, g, r.Root); err != nil {
					t.Fatal(err)
				}
			}
			linked := filepath.Join(t.TempDir(), "linked")
			r.Git("worktree", "add", "-q", "-b", "linked", linked)
			if _, err := initialize(ctx, g, linked, func(at string) error {
				if at == tc.stop {
					return errors.New("injected interruption")
				}
				return nil
			}); err == nil {
				t.Fatal("fault missed")
			}
			common := filepath.Join(r.Root, ".git")
			d, err := storage.OpenDir(filepath.Join(common, "gitlocal"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var j initJournal
			if err := d.ReadJSON("transactions/init.json", &j); err != nil {
				t.Fatal(err)
			}
			id := j.Identity.WorkspaceID
			if _, err := AbandonInit(ctx, g, r.Root, id); !errors.Is(err, storage.ErrConflict) {
				t.Fatalf("abandon accepted a resumable initialization: %v", err)
			}
			r.Git("worktree", "remove", "--force", linked)
			if _, err := Init(ctx, g, r.Root); err == nil || !strings.Contains(err.Error(), linked) {
				t.Fatalf("expected the orphaned journal to block init: %v", err)
			}
			removed, err := AbandonInit(ctx, g, r.Root, id)
			if err != nil {
				t.Fatalf("abandon: %v", err)
			}
			if !strings.Contains(strings.Join(removed, " "), "workspaces/"+id) {
				t.Fatalf("published registration not removed: %v", removed)
			}
			if _, err := os.Lstat(filepath.Join(common, "gitlocal", "transactions", "init.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remains: %v", err)
			}
			var done initJournal
			if err := d.ReadJSON(completedInitPath(id), &done); err != nil || done.Outcome != "abandoned" {
				t.Fatalf("completed record: %+v %v", done, err)
			}
			w, err := Init(ctx, g, r.Root)
			if err != nil {
				t.Fatalf("init after abandon: %v", err)
			}
			if w.WorkspaceID == id {
				t.Fatal("abandoned identity was reused")
			}
			if _, err := AbandonInit(ctx, g, r.Root, id); err != nil {
				t.Fatalf("abandon isn't idempotent: %v", err)
			}
		})
	}
}

func TestAbandonInitRefusedAfterRepositoryMoved(t *testing.T) {
	r := testutil.New(t)
	r.Commit()
	g := runner(t, r)
	ctx := context.Background()
	// The identity is published last, so only the journal's removal remains.
	if _, err := initialize(ctx, g, r.Root, func(at string) error {
		if at == "identity" {
			return errors.New("injected interruption")
		}
		return nil
	}); err == nil {
		t.Fatal("fault missed")
	}
	common := filepath.Join(r.Root, ".git")
	var j initJournal
	d, err := storage.OpenDir(filepath.Join(common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	err = d.ReadJSON("transactions/init.json", &j)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	id := j.Identity.WorkspaceID
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(r.Root, moved); err != nil {
		t.Fatal(err)
	}
	r.Root = moved
	if _, err := AbandonInit(ctx, g, moved, id); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("abandon accepted the initialization of a moved checkout: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(moved, ".git", "gitlocal", "workspaces", id, "workspace.json")); err != nil {
		t.Fatalf("published registration removed: %v", err)
	}
	w, err := Init(ctx, g, moved)
	if err != nil {
		t.Fatalf("init of the moved checkout: %v", err)
	}
	if w.WorkspaceID != id || w.Root != moved {
		t.Fatalf("moved checkout identity: %s at %s, want %s at %s", w.WorkspaceID, w.Root, id, moved)
	}
	if _, err := os.Lstat(filepath.Join(moved, ".git", "gitlocal", "transactions", "init.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", err)
	}
	if again, err := Init(ctx, g, moved); err != nil || again.WorkspaceID != id {
		t.Fatalf("init after relocation: %+v %v", again, err)
	}
}
