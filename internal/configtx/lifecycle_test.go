package configtx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func findWorkspace(t *testing.T, entries []WorkspaceInfo, id string) WorkspaceInfo {
	t.Helper()
	for _, entry := range entries {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatal("workspace not listed", id)
	return WorkspaceInfo{}
}
func TestWorkspaceMoveOfflineRemoveAndRecreate(t *testing.T) {
	r, g, _ := fixture(t)
	r.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	r.Git("worktree", "add", "-qb", "linked", linked)
	w, err := workspace.Init(ctx, g, linked)
	if err != nil {
		t.Fatal(err)
	}
	write(t, linked, "local")
	if _, err = Change(ctx, g, linked, true, "local"); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	r.Git("worktree", "move", linked, moved)
	entries, err := ListWorkspaces(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	entry := findWorkspace(t, entries, w.WorkspaceID)
	if entry.State != "moved" || !entry.Available || !entry.Registered {
		t.Fatal(entry)
	}
	again, err := workspace.Init(ctx, g, moved)
	if err != nil || again.WorkspaceID != w.WorkspaceID {
		t.Fatal("move lost identity", again, err)
	}
	r.Git("worktree", "lock", moved)
	offline := moved + "-offline"
	if err = os.Rename(moved, offline); err != nil {
		t.Fatal(err)
	}
	entries, err = ListWorkspaces(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	entry = findWorkspace(t, entries, w.WorkspaceID)
	if entry.State != "unavailable" || !entry.Locked || !entry.Registered {
		t.Fatal(entry)
	}
	if _, err = Retire(ctx, g, r.Root, w.WorkspaceID); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("offline locked workspace retired", err)
	}
	if err = os.Rename(offline, moved); err != nil {
		t.Fatal(err)
	}
	r.Git("worktree", "unlock", moved)
	r.Git("worktree", "remove", "--force", moved)
	entries, err = ListWorkspaces(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if findWorkspace(t, entries, w.WorkspaceID).State != "missing" {
		t.Fatal(entries)
	}
	if _, err = Retire(ctx, g, r.Root, w.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = Retire(ctx, g, r.Root, w.WorkspaceID); err != nil {
		t.Fatal("retirement is not idempotent", err)
	}
	entries, err = ListWorkspaces(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if findWorkspace(t, entries, w.WorkspaceID).State != "retained" {
		t.Fatal(entries)
	}
	r.Git("worktree", "add", "-qb", "recreated", moved)
	replacement, err := workspace.Init(ctx, g, moved)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.WorkspaceID == w.WorkspaceID {
		t.Fatal("recreated root reused identity")
	}
	entries, err = ListWorkspaces(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if findWorkspace(t, entries, w.WorkspaceID).Registered {
		t.Fatal("retained identity matched recreated path")
	}
}
func TestPruneRetiresOnlyMissingWorkspaces(t *testing.T) {
	r, g, _ := fixture(t)
	r.Commit()
	ctx := context.Background()
	ids := []string{}
	for _, name := range []string{"first", "second"} {
		linked := filepath.Join(t.TempDir(), name)
		r.Git("worktree", "add", "-qb", name, linked)
		w, err := workspace.Init(ctx, g, linked)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, w.WorkspaceID)
		r.Git("worktree", "remove", "--force", linked)
	}
	sort.Strings(ids)
	preview, err := Prune(ctx, g, r.Root, true)
	if err != nil || !reflect.DeepEqual(preview.Candidates, ids) || len(preview.Retired) != 0 {
		t.Fatal(preview, err)
	}
	for _, id := range ids {
		entries, e := ListWorkspaces(ctx, g, r.Root)
		if e != nil || findWorkspace(t, entries, id).State != "missing" {
			t.Fatal("dry-run changed workspace", id, e)
		}
	}
	result, err := Prune(ctx, g, r.Root, false)
	if err != nil || !reflect.DeepEqual(result.Retired, ids) {
		t.Fatal(result, err)
	}
	entries, err := ListWorkspaces(ctx, g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if findWorkspace(t, entries, id).State != "retained" {
			t.Fatal("prune lost retained workspace", id)
		}
	}
	again, err := Prune(ctx, g, r.Root, false)
	if err != nil || len(again.Candidates) != 0 || len(again.Retired) != 0 {
		t.Fatal("prune was not idempotent", again, err)
	}
}
func TestRetirementRecoveryAndSharedRules(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		for _, stage := range []string{"journal", "union", "selection", "final-exclude"} {
			t.Run(stage+map[bool]string{false: "-resume", true: "-rollback"}[rollback], func(t *testing.T) {
				t.Parallel()
				r, g, main := fixture(t)
				r.Commit()
				ctx := context.Background()
				linked := filepath.Join(t.TempDir(), "linked")
				r.Git("worktree", "add", "-qb", "linked", linked)
				other, err := workspace.Init(ctx, g, linked)
				if err != nil {
					t.Fatal(err)
				}
				write(t, r.Root, "shared")
				write(t, linked, "shared")
				write(t, linked, "unique")
				if _, err = Change(ctx, g, r.Root, true, "shared"); err != nil {
					t.Fatal(err)
				}
				if _, err = Change(ctx, g, linked, true, "shared", "unique"); err != nil {
					t.Fatal(err)
				}
				selectedBefore, err := os.ReadFile(filepath.Join(main.Common, "gitlocal", selectionPath(other.WorkspaceID)))
				if err != nil {
					t.Fatal(err)
				}
				r.Git("worktree", "remove", "--force", linked)
				result, err := retire(ctx, g, r.Root, other.WorkspaceID, func(at string) error {
					if at == stage {
						return errors.New("interrupted")
					}
					return nil
				})
				if err == nil {
					t.Fatal("missed retirement fault")
				}
				if _, err = Recover(ctx, g, r.Root, result.TransactionID, rollback); err != nil {
					t.Fatal(err)
				}
				if _, err = Recover(ctx, g, r.Root, result.TransactionID, rollback); err != nil {
					t.Fatal("recovery not idempotent", err)
				}
				selectedAfter, err := os.ReadFile(filepath.Join(main.Common, "gitlocal", selectionPath(other.WorkspaceID)))
				if err != nil || !bytes.Equal(selectedBefore, selectedAfter) {
					t.Fatal("retirement altered saved selection", err)
				}
				got := ignored(t, g, main, "shared", "unique")
				if !got["shared"] || got["unique"] != rollback {
					t.Fatal("incorrect shared exclusions", got)
				}
				entries, err := ListWorkspaces(ctx, g, r.Root)
				if err != nil {
					t.Fatal(err)
				}
				state := findWorkspace(t, entries, other.WorkspaceID).State
				if rollback && state != "missing" || !rollback && state != "retained" {
					t.Fatal("wrong recovered state", state)
				}
			})
		}
	}
}

func TestRetirementResumeRechecksRegistration(t *testing.T) {
	r, g, _ := fixture(t)
	r.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	r.Git("worktree", "add", "-qb", "linked", linked)
	w, err := workspace.Init(ctx, g, linked)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.ReadFile(filepath.Join(w.Admin, "gitlocal-workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Git("worktree", "remove", "--force", linked)
	result, err := retire(ctx, g, r.Root, w.WorkspaceID, func(at string) error {
		if at == "union" {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil {
		t.Fatal("missed fault")
	}
	r.Git("worktree", "add", "-qb", "returned", linked)
	admin, err := g.AdminAt(ctx, linked)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(admin, "gitlocal-workspace.json"), identity, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Recover(ctx, g, r.Root, result.TransactionID, false); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("retired a newly registered identity", err)
	}
	if _, err = Recover(ctx, g, r.Root, result.TransactionID, true); err != nil {
		t.Fatal("could not roll back retirement", err)
	}
}
