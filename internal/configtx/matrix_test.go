package configtx

import (
	"context"
	"errors"
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

// resolveMatrix applies first (resume or rollback) and falls back to abandon.
// Some direction must clear the journal and unblock the guards.
func resolveMatrix(t *testing.T, g *gitexec.Runner, root, id string, rollback bool, recover func(bool) error) string {
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
		how = "abandon"
		if _, err := Abandon(ctx, g, root, id); err != nil {
			t.Fatalf("%s failed and abandon failed too: %v", how, err)
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
				how := resolveMatrix(t, g, r.Root, result.TransactionID, rollback, func(back bool) error {
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
				resolveMatrix(t, r, linked, result.TransactionID, rollback, func(back bool) error {
					_, err := RecoverSeed(ctx, r, linked, result.TransactionID, back)
					return err
				})
			})
		}
	}
}
