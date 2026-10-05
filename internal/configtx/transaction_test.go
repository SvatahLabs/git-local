package configtx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func fixture(t *testing.T) (*testutil.Repo, *gitexec.Runner, *workspace.Context) {
	t.Helper()
	r := testutil.New(t)
	g, err := gitexec.New(r.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	w, err := workspace.Init(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	return r, g, w
}
func write(t *testing.T, root, path string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("private\x00\xff"), 0600); err != nil {
		t.Fatal(err)
	}
}
func load(t *testing.T, w *workspace.Context) selection.State {
	t.Helper()
	d, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var s selection.State
	if err = d.ReadJSON(selectionPath(w.WorkspaceID), &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func ignored(t *testing.T, g *gitexec.Runner, w *workspace.Context, paths ...string) map[string]bool {
	t.Helper()
	got, err := g.CheckIgnore(context.Background(), w.Root, w.Admin, paths)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func TestTrackUntrackAndUserIgnorePreservation(t *testing.T) {
	r, g, w := fixture(t)
	ctx := context.Background()
	write(t, r.Root, "notes/private")
	write(t, r.Root, "notes/public")
	write(t, r.Root, "notes/space name")
	os.MkdirAll(filepath.Join(w.Common, "info"), 0700)
	user := []byte("# preserve\r\n*.user\nnotes/private\n# no newline")
	os.WriteFile(w.Exclude, user, 0640)
	result, err := Change(ctx, g, r.Root, true, "notes")
	if err != nil || !result.Changed {
		t.Fatal(result, err)
	}
	if err = os.WriteFile(filepath.Join(r.Root, "notes/future"), []byte("future"), 0600); err != nil {
		t.Fatal(err)
	}
	got := ignored(t, g, w, "notes/private", "notes/public", "notes/future")
	for p, v := range got {
		if !v {
			t.Fatalf("%q not ignored", p)
		}
	}
	_, err = Change(ctx, g, r.Root, false, "notes/private")
	if err != nil {
		t.Fatal(err)
	}
	selected := load(t, w)
	if selected.Selected("notes/private") || !selected.Selected("notes/public") {
		t.Fatal("wrong persisted exclusion")
	}
	if !ignored(t, g, w, "notes/private")["notes/private"] {
		t.Fatal("untrack overrode the user's own ignore")
	}
	bytesAfter, _ := os.ReadFile(w.Exclude)
	ignoreCase, err := g.IgnoreCase(context.Background(), w.Root, w.Admin)
	if err != nil {
		t.Fatal(err)
	}
	body, err := exclude.Compile([]selection.State{selected}, ignoreCase)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := exclude.UserBytes(bytesAfter, body)
	if err != nil || !bytes.Equal(outside, user) {
		t.Fatal("user bytes changed", err)
	}
	fi, _ := os.Stat(w.Exclude)
	if fi.Mode().Perm() != 0640 && os.PathSeparator != '\\' {
		t.Fatal("ignore permissions changed")
	}
	before := selected.Generation
	result, err = Change(ctx, g, r.Root, true, "notes")
	if err != nil || result.Changed || load(t, w).Generation != before {
		t.Fatal("retracking parent changed exclusions", err)
	}
	_, err = Change(ctx, g, r.Root, true, "notes/private")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Change(ctx, g, r.Root, false, "notes")
	if err != nil {
		t.Fatal(err)
	}
	// The block keeps only the staging-file rule (GL-082); user bytes are
	// restored unchanged around it.
	after, _ := os.ReadFile(w.Exclude)
	if !bytes.Equal(after, exclude.Render(user, nil)) {
		t.Fatal("last untrack did not restore user bytes")
	}
	if _, err = os.Stat(filepath.Join(r.Root, "notes/private")); err != nil {
		t.Fatal("untrack removed working file")
	}
}
func TestIgnoreOverrideRollsBackWithoutLosingUserText(t *testing.T) {
	for _, where := range []string{"local", "gitignore"} {
		t.Run(where, func(t *testing.T) {
			r, g, w := fixture(t)
			write(t, r.Root, "secret")
			os.MkdirAll(filepath.Join(w.Common, "info"), 0700)
			user := []byte("# mine\n")
			if where == "local" {
				user = append(user, []byte("!secret\n")...)
			} else {
				os.WriteFile(filepath.Join(r.Root, ".gitignore"), []byte("!secret\n"), 0600)
			}
			os.WriteFile(w.Exclude, user, 0600)
			_, err := Change(context.Background(), g, r.Root, true, "secret")
			if !errors.Is(err, storage.ErrConflict) {
				t.Fatalf("override accepted: %v", err)
			}
			if len(load(t, w).Include) != 0 {
				t.Fatal("rejected track published selection")
			}
			after, _ := os.ReadFile(w.Exclude)
			if !bytes.Equal(after, user) {
				t.Fatal("rollback lost user text")
			}
			pending, err := Pending(context.Background(), g, r.Root)
			if err != nil || len(pending) != 0 {
				t.Fatal("ordinary rejection left a blocked transaction", err)
			}
		})
	}
}
func TestSharedUnionAndUnavailableWorktree(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	linked := filepath.Join(r.Root, "linked")
	r.Git("worktree", "add", "-qb", "other", linked)
	other, err := workspace.Init(context.Background(), g, linked)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{r.Root, linked} {
		write(t, root, "notes/private")
		write(t, root, "notes/public")
	}
	ctx := context.Background()
	if _, err = Change(ctx, g, r.Root, true, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err = Change(ctx, g, linked, true, "notes/private"); err != nil {
		t.Fatal(err)
	}
	if _, err = Change(ctx, g, r.Root, false, "notes/private"); err != nil {
		t.Fatal(err)
	}
	if !ignored(t, g, w, "notes/private")["notes/private"] {
		t.Fatal("one workspace unignored the other's selection")
	}
	hidden := filepath.Join(r.Root, "offline")
	if err = os.Rename(linked, hidden); err != nil {
		t.Fatal(err)
	}
	result, err := Change(ctx, g, r.Root, false, "notes")
	if err != nil || len(result.Warnings) == 0 {
		t.Fatal("unavailable worktree blocked mutation or was not reported", err)
	}
	if !ignored(t, g, w, "notes/private")["notes/private"] {
		t.Fatal("unavailable workspace lost its protection")
	}
	if len(load(t, other).Include) == 0 {
		t.Fatal("unavailable selection removed")
	}
	// Restore the registration's path; its next operation must verify its state.
	if err = os.Rename(hidden, linked); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(linked, ".gitignore"), []byte("!notes/private\n"), 0600)
	if _, err = Change(ctx, g, linked, true, "notes/private"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("returning workspace was not revalidated", err)
	}
}
func TestConfigurationRecoveryAtEveryPublication(t *testing.T) {
	stages := []string{"journal", "before-union", "union", "before-selection", "selection", "before-final-exclude", "final-exclude", "committed", "before-archive", "archived"}
	for _, stage := range stages {
		for _, rollback := range []bool{false, true} {
			if rollback && (stage == "committed" || stage == "before-archive" || stage == "archived") {
				continue
			}
			name := stage + "-resume"
			if rollback {
				name = stage + "-rollback"
			}
			t.Run(name, func(t *testing.T) {
				r, g, w := fixture(t)
				write(t, r.Root, "local")
				ctx := context.Background()
				result, err := change(ctx, g, r.Root, true, func(s string) error {
					if s == stage {
						return errors.New("injected interruption")
					}
					return nil
				}, "local")
				if err == nil {
					t.Fatal("fault missed")
				}
				if _, err = Recover(ctx, g, r.Root, result.TransactionID, rollback); err != nil {
					t.Fatal(err)
				}
				if _, err = Recover(ctx, g, r.Root, result.TransactionID, rollback); err != nil {
					t.Fatal("recovery not idempotent", err)
				}
				state := load(t, w)
				if state.Selected("local") == rollback {
					t.Fatal("wrong recovered state")
				}
				if got := ignored(t, g, w, "local")["local"]; got == rollback {
					t.Fatal("wrong recovered ignore rules")
				}
				pending, err := Pending(ctx, g, r.Root)
				if err != nil || len(pending) != 0 {
					t.Fatal("pending transaction remained", err)
				}
			})
		}
	}
}
func TestExternalEditsStopRecovery(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, "local")
	ctx := context.Background()
	result, err := change(ctx, g, r.Root, true, func(s string) error {
		if s == "union" {
			return errors.New("stop")
		}
		return nil
	}, "local")
	if err == nil {
		t.Fatal("fault missed")
	}
	before, _ := os.ReadFile(w.Exclude)
	edited := append(before, []byte("# external edit\n")...)
	os.WriteFile(w.Exclude, edited, 0600)
	for _, rollback := range []bool{false, true} {
		if _, err = Recover(ctx, g, r.Root, result.TransactionID, rollback); !errors.Is(err, storage.ErrConflict) {
			t.Fatal("external edit overwritten", err)
		}
	}
	after, _ := os.ReadFile(w.Exclude)
	if !bytes.Equal(after, edited) {
		t.Fatal("recovery changed external bytes")
	}
	if _, err = Change(ctx, g, r.Root, true, "local"); !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), result.TransactionID) {
		t.Fatal("new mutation did not report pending ID", err)
	}
	os.WriteFile(w.Exclude, before, 0600)
	if _, err = Recover(ctx, g, r.Root, result.TransactionID, true); err != nil {
		t.Fatal(err)
	}
}
func TestUntrackNowOwnedPathAndSchemaMigration(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, "local")
	ctx := context.Background()
	if _, err := Change(ctx, g, r.Root, true, "local"); err != nil {
		t.Fatal(err)
	}
	r.Git("add", "-f", "local")
	if _, err := Change(ctx, g, r.Root, false, "local"); err != nil {
		t.Fatal("ownership blocked untracking", err)
	}
	// A legacy include-only empty state is migrated only with a transaction.
	file := filepath.Join(w.Common, "gitlocal", selectionPath(w.WorkspaceID))
	os.WriteFile(file, []byte(`{"schemaVersion":1,"generation":4,"include":[]}`), 0600)
	result, err := Change(ctx, g, r.Root, false, "not-selected")
	if err != nil || !result.Changed {
		t.Fatal("legacy migration failed", err)
	}
	after := load(t, w)
	if after.SchemaVersion != 2 || after.Generation != 4 {
		t.Fatal("migration changed semantic generation")
	}
}

func TestInterruptedRollback(t *testing.T) {
	for _, stage := range []string{"rollback-intent", "before-rollback-union", "rollback-union", "before-rollback-selection", "rollback-selection", "before-rollback-exclude", "rollback-exclude", "rolledback"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			r, g, w := fixture(t)
			ctx := context.Background()
			write(t, r.Root, "local")
			stop := func(at string) fault {
				return func(s string) error {
					if s == at {
						return errors.New("interrupted")
					}
					return nil
				}
			}
			result, err := change(ctx, g, r.Root, true, stop("selection"), "local")
			if err == nil {
				t.Fatal("fault missed")
			}
			if _, err = recoverTransaction(ctx, g, r.Root, result.TransactionID, true, stop(stage)); err == nil {
				t.Fatal("rollback fault missed")
			}
			if _, err = Recover(ctx, g, r.Root, result.TransactionID, false); err == nil {
				t.Fatal("resume reversed durable rollback intent")
			}
			if _, err = Recover(ctx, g, r.Root, result.TransactionID, true); err != nil {
				t.Fatal(err)
			}
			if load(t, w).Selected("local") {
				t.Fatal("rollback left selection")
			}
			if _, err = workspace.Init(ctx, g, r.Root); err != nil {
				t.Fatal("completed transaction blocked init", err)
			}
		})
	}
}

func TestExternalSelectionEditStopsRecovery(t *testing.T) {
	r, g, w := fixture(t)
	ctx := context.Background()
	write(t, r.Root, "local")
	result, err := change(ctx, g, r.Root, true, func(s string) error {
		if s == "union" {
			return errors.New("stop")
		}
		return nil
	}, "local")
	if err == nil {
		t.Fatal("fault missed")
	}
	path := filepath.Join(w.Common, "gitlocal", selectionPath(w.WorkspaceID))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	external := append(bytes.Clone(before), ' ')
	if err = os.WriteFile(path, external, 0600); err != nil {
		t.Fatal(err)
	}
	for _, rollback := range []bool{false, true} {
		if _, err = Recover(ctx, g, r.Root, result.TransactionID, rollback); !errors.Is(err, storage.ErrConflict) {
			t.Fatal("external selection accepted", err)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, external) {
		t.Fatal("external selection overwritten")
	}
}
