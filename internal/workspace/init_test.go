package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
)

func runner(t *testing.T, r *testutil.Repo) *gitexec.Runner {
	t.Helper()
	g, err := gitexec.New(r.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}
func TestInitIdempotentAndIsolated(t *testing.T) {
	r := testutil.New(t)
	r.Commit()
	g := runner(t, r)
	index, _ := os.ReadFile(filepath.Join(r.Root, ".git", "index"))
	refs := r.Git("show-ref")
	objects := r.Git("count-objects", "-v")
	app, _ := os.ReadFile(filepath.Join(r.Root, "app.txt"))
	exclude, _ := os.ReadFile(filepath.Join(r.Root, ".git", "info", "exclude"))
	w, err := Init(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !storage.ValidUUID(w.WorkspaceID) || !storage.ValidUUID(w.RepositoryID) {
		t.Fatal("invalid IDs")
	}
	sub := filepath.Join(r.Root, "sub")
	os.Mkdir(sub, 0700)
	again, err := Init(context.Background(), g, sub)
	if err != nil {
		t.Fatal(err)
	}
	if w.WorkspaceID != again.WorkspaceID {
		t.Fatal("identity changed")
	}
	after, _ := os.ReadFile(filepath.Join(r.Root, ".git", "index"))
	if !bytes.Equal(index, after) || !bytes.Equal(refs, r.Git("show-ref")) || !bytes.Equal(objects, r.Git("count-objects", "-v")) {
		t.Fatal("main Git state changed")
	}
	after, _ = os.ReadFile(filepath.Join(r.Root, "app.txt"))
	if !bytes.Equal(app, after) {
		t.Fatal("tracked bytes changed")
	}
	after, _ = os.ReadFile(filepath.Join(r.Root, ".git", "info", "exclude"))
	if !bytes.Equal(exclude, after) {
		t.Fatal("exclusions changed")
	}
	if _, err = g.History(context.Background(), w.Store, nil, "rev-parse", "--is-bare-repository"); err != nil {
		t.Fatal(err)
	}
	r.Git("checkout", "--detach", "-q")
	again, err = Init(context.Background(), g, r.Root)
	if err != nil || again.WorkspaceID != w.WorkspaceID {
		t.Fatalf("detached identity: %v", err)
	}
}
func TestUnbornAndWorktreeLifecycle(t *testing.T) {
	r := testutil.New(t)
	g := runner(t, r)
	ctx := context.Background()
	main, err := Init(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if main.Head != "" {
		t.Fatal("unborn HEAD not empty")
	}
	r.Commit()
	linked := filepath.Join(r.Root, "linked")
	r.Git("worktree", "add", "-qb", "feature", linked)
	w, err := Init(ctx, g, linked)
	if err != nil {
		t.Fatal(err)
	}
	if w.WorkspaceID == main.WorkspaceID || w.RepositoryID != main.RepositoryID {
		t.Fatal("worktree identity not independent")
	}
	moved := filepath.Join(r.Root, "moved")
	r.Git("worktree", "move", linked, moved)
	again, err := Init(ctx, g, moved)
	if err != nil || again.WorkspaceID != w.WorkspaceID {
		t.Fatalf("move: %v", err)
	}
	r.Git("worktree", "remove", moved)
	if _, err = os.Stat(filepath.Join(main.Common, "gitlocal", "workspaces", w.WorkspaceID, "workspace.json")); err != nil {
		t.Fatal(err)
	}
	r.Git("worktree", "add", "-q", moved, "feature")
	newW, err := Init(ctx, g, moved)
	if err != nil || newW.WorkspaceID == w.WorkspaceID {
		t.Fatalf("recreated identity: %v", err)
	}
}
func TestInitRecoversEveryPublicationBoundary(t *testing.T) {
	stages := []string{"journal", "history.git/objects/info", "history.git/objects/pack", "history.git/refs/heads", "history.git/refs/tags", "history.git/HEAD", "history.git/config", "store", "workspace", "selection", "repository", "identity"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			r := testutil.New(t)
			g := runner(t, r)
			sentinel := errors.New("injected interruption")
			_, err := initialize(context.Background(), g, r.Root, func(s string) error {
				if s == stage {
					return sentinel
				}
				return nil
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("injection: %v", err)
			}
			var j initJournal
			d, e := storage.OpenDir(filepath.Join(r.Root, ".git", "gitlocal"))
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			if e = d.ReadJSON("transactions/init.json", &j); e != nil {
				t.Fatal(e)
			}
			// New runner represents a fresh process, including capability revalidation.
			fresh := runner(t, r)
			w, err := Init(context.Background(), fresh, r.Root)
			if err != nil {
				t.Fatal(err)
			}
			if w.WorkspaceID != j.Identity.WorkspaceID {
				t.Fatal("resume changed identity")
			}
			if _, e = d.Lstat("transactions/init.json"); !os.IsNotExist(e) {
				t.Fatal("journal not removed")
			}
		})
	}
}
func TestUnknownStateAndFutureSchemaFailClosed(t *testing.T) {
	for _, name := range []string{"unknown", "future", "partial", "symlink"} {
		t.Run(name, func(t *testing.T) {
			r := testutil.New(t)
			g := runner(t, r)
			base := filepath.Join(r.Root, ".git", "gitlocal")
			os.MkdirAll(base, 0700)
			switch name {
			case "unknown":
				os.WriteFile(filepath.Join(base, "other"), []byte("keep"), 0600)
			case "future":
				os.WriteFile(filepath.Join(base, "repository.json"), []byte(`{"schemaVersion":999,"repositoryId":"unknown"}`), 0600)
			case "partial":
				os.Mkdir(filepath.Join(base, "history.git"), 0700)
			case "symlink":
				target := t.TempDir()
				if err := os.Symlink(target, filepath.Join(base, "history.git")); err != nil {
					t.Skip(err)
				}
			}
			if _, err := Init(context.Background(), g, r.Root); err == nil {
				t.Fatal("accepted unsafe state")
			}
		})
	}
}
func TestDuplicateIdentityRejected(t *testing.T) {
	r := testutil.New(t)
	r.Commit()
	g := runner(t, r)
	main, err := Init(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(r.Root, "linked")
	r.Git("worktree", "add", "-qb", "feature", linked)
	w, err := Discover(context.Background(), g, linked)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(main.Admin, "gitlocal-workspace.json"))
	os.WriteFile(filepath.Join(w.Admin, "gitlocal-workspace.json"), b, 0600)
	if _, err = Init(context.Background(), g, r.Root); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("wanted duplicate conflict, got %v", err)
	}
}
func TestLayouts(t *testing.T) {
	t.Run("exposed", func(t *testing.T) {
		r := testutil.New(t)
		repo := filepath.Join(r.Root, "exposed")
		os.Mkdir(repo, 0700)
		r.Git("init", "-q", "--separate-git-dir", filepath.Join(repo, "metadata"), repo)
		if _, err := Init(context.Background(), runner(t, r), repo); err == nil {
			t.Fatal("accepted exposed metadata")
		}
	})
	t.Run("external", func(t *testing.T) {
		r := testutil.New(t)
		repo := filepath.Join(r.Root, "external")
		os.Mkdir(repo, 0700)
		meta := filepath.Join(t.TempDir(), "metadata")
		r.Git("init", "-q", "--separate-git-dir", meta, repo)
		if _, err := Init(context.Background(), runner(t, r), repo); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bare", func(t *testing.T) {
		r := testutil.New(t)
		bare := filepath.Join(r.Root, "bare.git")
		r.Git("init", "--bare", "-q", bare)
		if _, err := Init(context.Background(), runner(t, r), bare); err == nil {
			t.Fatal("accepted bare")
		}
	})
	t.Run("submodule", func(t *testing.T) {
		source := testutil.New(t)
		source.Commit()
		r := testutil.New(t)
		r.Commit()
		r.Git("-c", "protocol.file.allow=always", "submodule", "add", "-q", source.Root, "module")
		w, err := Init(context.Background(), runner(t, r), filepath.Join(r.Root, "module"))
		if err != nil {
			t.Fatal(err)
		}
		if w.Root == r.Root {
			t.Fatal("submodule used parent identity")
		}
	})
}
func TestResumeDoesNotOverwriteExternalMetadata(t *testing.T) {
	r := testutil.New(t)
	g := runner(t, r)
	_, err := initialize(context.Background(), g, r.Root, func(s string) error {
		if s == "workspace" {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault not reached")
	}
	entries, _ := os.ReadDir(filepath.Join(r.Root, ".git", "gitlocal", "workspaces"))
	p := filepath.Join(r.Root, ".git", "gitlocal", "workspaces", entries[0].Name(), "workspace.json")
	changed := []byte(`{"external":"keep"}`)
	os.WriteFile(p, changed, 0600)
	if _, err = Init(context.Background(), g, r.Root); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	after, _ := os.ReadFile(p)
	if !reflect.DeepEqual(changed, after) {
		t.Fatal("external edits overwritten")
	}
}

func TestConcurrentInitPublishesOneIdentity(t *testing.T) {
	r := testutil.New(t)
	a, b := runner(t, r), runner(t, r)
	type result struct {
		w   *Context
		err error
	}
	results := make(chan result, 2)
	for _, g := range []*gitexec.Runner{a, b} {
		go func(g *gitexec.Runner) { w, err := Init(context.Background(), g, r.Root); results <- result{w, err} }(g)
	}
	x, y := <-results, <-results
	if x.err != nil || y.err != nil {
		t.Fatalf("concurrent init: %v, %v", x.err, y.err)
	}
	if x.w.WorkspaceID != y.w.WorkspaceID {
		t.Fatal("concurrent initializers published different identities")
	}
}
func TestInterruptedAtomicTempsDoNotPreventResume(t *testing.T) {
	r := testutil.New(t)
	g := runner(t, r)
	_, err := initialize(context.Background(), g, r.Root, func(s string) error {
		if s == "journal" {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault not reached")
	}
	base := filepath.Join(r.Root, ".git", "gitlocal")
	id, _ := storage.UUID()
	temp := ".gitlocal-tmp-" + id
	os.WriteFile(filepath.Join(base, "transactions", temp), []byte("partial journal"), 0600)
	os.WriteFile(filepath.Join(base, temp), []byte("partial metadata"), 0600)
	if _, err = Init(context.Background(), g, r.Root); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(base, temp)); err != nil || string(b) != "partial metadata" {
		t.Fatal("orphan temp was modified")
	}
}
func TestExplicitGitDirectoryContext(t *testing.T) {
	r := testutil.New(t)
	env := append(r.RunnerEnv(), "GIT_DIR="+filepath.Join(r.Root, ".git"), "GIT_WORK_TREE="+r.Root)
	g, err := gitexec.New(env)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	w, err := Init(context.Background(), g, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != r.Root {
		t.Fatal("lost explicit worktree context")
	}
}

func TestLegacySelectionRemainsReadableWithoutRewrite(t *testing.T) {
	r := testutil.New(t)
	g := runner(t, r)
	w, err := Init(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.Common, "gitlocal", "workspaces", w.WorkspaceID, "selection.json")
	legacy := []byte("{\"schemaVersion\":1,\"generation\":7,\"include\":[]}\n")
	if err = os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Init(context.Background(), g, r.Root); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, legacy) {
		t.Fatal("init silently migrated selection")
	}
}
func TestInitReplaysLegacySelectionJournal(t *testing.T) {
	r := testutil.New(t)
	g := runner(t, r)
	_, err := initialize(context.Background(), g, r.Root, func(s string) error {
		if s == "selection" {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault not reached")
	}
	d, err := storage.OpenDir(filepath.Join(r.Root, ".git", "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var journal initJournal
	if err = d.ReadJSON("transactions/init.json", &journal); err != nil {
		t.Fatal(err)
	}
	journal.SelectionSchema = 0
	if err = d.WriteJSON("transactions/init.json", journal); err != nil {
		t.Fatal(err)
	}
	if err = d.AtomicWrite(filepath.Join("workspaces", journal.Identity.WorkspaceID, "selection.json"), []byte(`{"schemaVersion":1,"generation":0,"include":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Init(context.Background(), g, r.Root); err != nil {
		t.Fatal(err)
	}
}
