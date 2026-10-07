package configtx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/selection"
	"gitlocal/internal/workspace"
)

// seedInto prepares a linked worktree and a planner that seeds notes/a.
func seedInto(t *testing.T) (context.Context, string, *workspace.Context, func(workspace.Context) (SeedPlan, error), func(string, func(string) error) (SeedResult, error)) {
	t.Helper()
	repo, r, main := fixture(t)
	repo.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.History(ctx, w.Store, strings.NewReader("seeded content"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(blob))
	planner := func(_ workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes/a", Kind: "file"}}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: id, Selection: state, Files: []SeedFile{{Path: "notes/a", Mode: "100644", Blob: id}}}, nil
	}
	run := func(transaction string, fault func(string) error) (SeedResult, error) {
		if transaction != "" {
			return RecoverSeed(ctx, r, linked, transaction, true)
		}
		return seed(ctx, r, linked, planner, fault)
	}
	return ctx, linked, w, planner, run
}

// TestSeedIntoEmptyLegacySelection: a workspace whose initialization started
// under an earlier release has a schema-1 selection file with no exclude list.
// It is still empty, so seeding into it must work.
func TestSeedIntoEmptyLegacySelection(t *testing.T) {
	_, linked, w, _, run := seedInto(t)
	legacy := []byte(`{"schemaVersion":1,"generation":0,"include":[]}` + "\n")
	if err := os.WriteFile(filepath.Join(w.Common, "gitlocal", "workspaces", w.WorkspaceID, "selection.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("", nil); err != nil {
		t.Fatalf("seed into an empty legacy selection: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(linked, "notes", "a")); err != nil || string(data) != "seeded content" {
		t.Fatalf("seeded file: %q %v", data, err)
	}
	if got := load(t, w).Include; len(got) != 1 || got[0].Path != "notes/a" {
		t.Fatalf("selection: %+v", got)
	}
}
