package restore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/configtx"
)

func TestAbandonRestoreKeepsUserEditsAndQuarantinesUnknownStage(t *testing.T) {
	t.Parallel()
	repo, r, w := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "baseline")
	save(t, r, root)
	result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
		if at == "stage:0" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault missed")
	}
	meta, err := openMeta(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	j, err := readJournal(meta, journalPath(result.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	apply, rollback := j.Manifest.Stages[0].Apply, j.Manifest.Stages[0].Rollback
	// A new user edit at the restore path and unknown bytes at a reserved stage
	// stop both directions.
	write(t, root, "notes/a", "user edit")
	write(t, root, rollback, "unknown")
	for _, back := range []bool{false, true} {
		if _, err := Recover(ctx, r, root, result.TransactionID, back); err == nil {
			t.Fatalf("recovery (rollback=%v) discarded a user edit", back)
		}
	}
	abandoned, err := Abandon(ctx, r, root, result.TransactionID)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if len(abandoned.Removed) != 1 || abandoned.Removed[0] != apply || len(abandoned.Quarantined) != 1 || abandoned.Quarantined[0].Source != rollback {
		t.Fatalf("abandon result: %+v", abandoned)
	}
	kept, err := os.ReadFile(filepath.Join(abandoned.Quarantine, filepath.FromSlash(abandoned.Quarantined[0].File)))
	if err != nil || string(kept) != "unknown" {
		t.Fatalf("quarantined bytes: %q %v", kept, err)
	}
	for _, stage := range []string{apply, rollback} {
		if _, err := os.Lstat(filepath.Join(root, stage)); !os.IsNotExist(err) {
			t.Fatalf("stage %s remains: %v", stage, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "notes/a")); err != nil || string(data) != "user edit" {
		t.Fatalf("user file changed: %q %v", data, err)
	}
	// The recovery ref keeps the anchored images; the completed journal still
	// validates for doctor and migration.
	if _, err := r.History(ctx, w.Store, nil, "rev-parse", "--verify", RecoveryRef(w.WorkspaceID, result.TransactionID)); err != nil {
		t.Fatalf("recovery ref removed: %v", err)
	}
	done, err := readJournal(meta, completedPath(result.TransactionID))
	if err != nil || done.Outcome != configtx.OutcomeAbandoned {
		t.Fatalf("completed journal: %+v %v", done.Outcome, err)
	}
	if err := InspectJournal(ctx, r, *w, j.Manifest.Plan.Selection, j.Manifest.ConfigDigest, result.TransactionID, true, []string{w.WorkspaceID}); err != nil {
		t.Fatalf("abandoned journal doesn't validate: %v", err)
	}
	if _, err := configtx.LoadGuardSnapshot(ctx, r, root); err != nil {
		t.Fatalf("guards still blocked: %v", err)
	}
	save(t, r, root)
	if _, err := Abandon(ctx, r, root, result.TransactionID); err != nil {
		t.Fatalf("abandon isn't idempotent: %v", err)
	}
}
