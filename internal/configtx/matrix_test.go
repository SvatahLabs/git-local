package configtx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/workspace"
)

// recordPoints runs op once without interruption and returns every fault point
// it passed, in order.
func recordPoints(t *testing.T, op func(fault) error) []string {
	t.Helper()
	var points []string
	if err := op(func(name string) error { points = append(points, name); return nil }); err != nil {
		t.Fatalf("uninterrupted run: %v", err)
	}
	return points
}

func stopAt(point string) fault {
	return func(name string) error {
		if name == point {
			return errors.New("injected interruption at " + point)
		}
		return nil
	}
}

// resolveMatrix recovers in the requested direction, which must clear the
// journal and unblock the guards; it doesn't fall back to abandon, so a
// resume or rollback that always fails fails the matrix (GL-123). Only a
// transaction that already committed refuses rollback, naming resume, which
// must then finish it; committed holds the points after the commit, where
// that refusal is accepted.
func resolveMatrix(t *testing.T, g *gitexec.Runner, root, id, point string, committed map[string]bool, rollback bool, recover func(bool) error) string {
	t.Helper()
	ctx := context.Background()
	pending, err := Pending(ctx, g, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		return "none"
	}
	if len(pending) != 1 || pending[0] != id {
		t.Fatalf("pending %v, want %s", pending, id)
	}
	how := "resume"
	if rollback {
		how = "rollback"
	}
	if err := recover(rollback); err != nil {
		if !rollback || !committed[point] || !strings.Contains(err.Error(), "already committed; use --resume") {
			t.Fatalf("%s failed: %v", how, err)
		}
		how = "resume"
		if err := recover(false); err != nil {
			t.Fatalf("rollback refused after the commit, and resume failed: %v", err)
		}
	}
	if pending, err := Pending(ctx, g, root); err != nil || len(pending) != 0 {
		t.Fatalf("after %s: pending %v %v", how, pending, err)
	}
	if _, err := LoadGuardSnapshot(ctx, g, root); err != nil {
		t.Fatalf("after %s: guards blocked: %v", how, err)
	}
	t.Logf("resolved by %s", how)
	return how
}

func TestEveryConfigurationFaultIsResolvable(t *testing.T) {
	ctx := context.Background()
	r, g, _ := fixture(t)
	write(t, r.Root, "notes/a")
	points := recordPoints(t, func(f fault) error { _, err := change(ctx, g, r.Root, true, f, "notes"); return err })
	for _, point := range points {
		for _, rollback := range []bool{false, true} {
			t.Run(point+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				t.Parallel()
				r, g, w := fixture(t)
				write(t, r.Root, "notes/a")
				result, err := change(ctx, g, r.Root, true, stopAt(point), "notes")
				if err == nil {
					t.Fatalf("fault %s missed", point)
				}
				how := resolveMatrix(t, g, r.Root, result.TransactionID, point, map[string]bool{"committed": true, "before-archive": true}, rollback, func(back bool) error {
					_, err := Recover(ctx, g, r.Root, result.TransactionID, back)
					return err
				})
				if how == "resume" && len(load(t, w).Include) != 1 || how == "rollback" && len(load(t, w).Include) != 0 {
					t.Fatalf("%s left selection %+v", how, load(t, w))
				}
			})
		}
	}
}

func TestEverySeedFaultIsResolvable(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (string, *gitexec.Runner, SeedPlanner) {
		repo, r, main := fixture(t)
		repo.Commit()
		linked := filepath.Join(t.TempDir(), "linked")
		repo.Git("worktree", "add", "-q", "-b", "seed", linked)
		w, err := workspace.Init(ctx, r, linked)
		if err != nil {
			t.Fatal(err)
		}
		blob := func(content string) string {
			out, err := r.History(ctx, w.Store, strings.NewReader(content), "hash-object", "-w", "--stdin", "--no-filters")
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(out))
		}
		a, b := blob("seeded-a"), blob("seeded-b")
		return linked, r, func(_ workspace.Context) (SeedPlan, error) {
			state := selection.Empty()
			state.Generation = 1
			state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
			return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: a, Selection: state, Files: []SeedFile{{Path: "notes/a", Mode: "100644", Blob: a}, {Path: "notes/sub/b", Mode: "100755", Blob: b}}}, nil
		}
	}
	linked, r, planner := setup(t)
	points := recordPoints(t, func(f fault) error { _, err := seed(ctx, r, linked, planner, f); return err })
	for _, point := range points {
		for _, rollback := range []bool{false, true} {
			t.Run(point+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				t.Parallel()
				linked, r, planner := setup(t)
				result, err := seed(ctx, r, linked, planner, stopAt(point))
				if err == nil {
					t.Fatalf("fault %s missed", point)
				}
				// No seed fault point follows the commit, so none may refuse rollback.
				how := resolveMatrix(t, r, linked, result.TransactionID, point, nil, rollback, func(back bool) error {
					_, err := RecoverSeed(ctx, r, linked, result.TransactionID, back)
					return err
				})
				// Resume seeds every file; rollback, or a stop before the
				// journal, leaves none.
				for name, want := range map[string]string{"notes/a": "seeded-a", "notes/sub/b": "seeded-b"} {
					b, err := os.ReadFile(filepath.Join(linked, filepath.FromSlash(name)))
					if how == "resume" && (err != nil || string(b) != want) || how != "resume" && !os.IsNotExist(err) {
						t.Fatalf("%s left %s: %q %v", how, name, b, err)
					}
				}
			})
		}
	}
}
