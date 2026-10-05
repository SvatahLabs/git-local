package configtx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/selection"
	"gitlocal/internal/workspace"
)

func TestSeedRecoveryResumesAndRollsBackCreatedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, stop string
		rollback   bool
	}{
		{"resume after journal", "seed-journal", false},
		{"rollback after file", "seed-file-0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, r, main := fixture(t)
			repo.Commit()
			ctx := context.Background()
			linked := filepath.Join(t.TempDir(), "linked")
			repo.Git("worktree", "add", "-q", "-b", "seed", linked)
			w, err := workspace.Init(ctx, r, linked)
			if err != nil {
				t.Fatal(err)
			}
			blob, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
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
			result, err := seed(ctx, r, linked, planner, func(name string) error {
				if name == tc.stop {
					return errors.New("injected interruption")
				}
				return nil
			})
			if err == nil || result.TransactionID == "" {
				t.Fatalf("seed did not stop at %s: %+v %v", tc.stop, result, err)
			}
			pending, err := Pending(ctx, r, linked)
			if err != nil || len(pending) != 1 || pending[0] != result.TransactionID {
				t.Fatalf("pending: %v %v", pending, err)
			}
			if _, err := workspace.Init(ctx, r, linked); err == nil || !strings.Contains(err.Error(), result.TransactionID) {
				t.Fatalf("init did not direct recovery: %v", err)
			}
			if err := ReadDiagnostic(ctx, r, linked, func(d DiagnosticState) error {
				if d.JournalErr != nil || len(d.Journals) != 1 || d.Journals[0].Kind != "seed" || d.Journals[0].Err != nil {
					t.Fatalf("seed diagnosis: %+v %v", d.Journals, d.JournalErr)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := RecoverSeed(ctx, r, linked, result.TransactionID, tc.rollback); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(linked, "notes", "a"))
			if tc.rollback {
				if !os.IsNotExist(err) || len(load(t, w).Include) != 0 {
					t.Fatalf("rollback left seed: %q %v", data, err)
				}
			} else if err != nil || string(data) != "seeded" || len(load(t, w).Include) != 1 {
				t.Fatalf("resume lost seed: %q %v", data, err)
			}
			pending, err = Pending(ctx, r, linked)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending after recovery: %v %v", pending, err)
			}
		})
	}
}
