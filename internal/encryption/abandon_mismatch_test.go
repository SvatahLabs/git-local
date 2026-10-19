package encryption

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/restore"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
)

// TestAbandonWithoutKeyQuarantinesMismatchedJournal: after publication,
// abandon updates completed journals for the encrypted history. A journal an
// outside edit changed matches neither version, and the remedy resume needs
// the key, often the reason to abandon. Abandon must finish without it by
// quarantining that journal, and leave a state doctor explains.
func TestAbandonWithoutKeyQuarantinesMismatchedJournal(t *testing.T) {
	r, w, root, key, restoreID, _ := restoredMigrationFixture(t)
	ctx := context.Background()
	id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
		if point == "policy-published" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault missed")
	}
	completed := filepath.Join(w.Common, "gitlocal", "transactions", "completed", "restore-"+restoreID+".json")
	data, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	edited := append(data, '\n')
	if err := os.WriteFile(completed, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(key, key+".away"); err != nil {
		t.Fatal(err)
	}
	result, err := AbandonMigration(ctx, r, root, id)
	if err != nil {
		t.Fatalf("abandon without the key: %v", err)
	}
	var entry *configtx.QuarantineEntry
	for i := range result.Quarantined {
		if result.Quarantined[i].Source == "transactions/completed/restore-"+restoreID+".json" {
			entry = &result.Quarantined[i]
		}
	}
	if entry == nil || entry.Kind != "journal" || !strings.Contains(strings.Join(result.Warnings, "\n"), "restore-"+restoreID+".json") {
		t.Fatalf("abandon result: %+v", result)
	}
	if kept, err := os.ReadFile(filepath.Join(result.Quarantine, filepath.FromSlash(entry.File))); err != nil || string(kept) != string(edited) {
		t.Fatalf("quarantined journal: %q %v", kept, err)
	}
	if _, err := os.Stat(completed); !os.IsNotExist(err) {
		t.Fatalf("mismatched journal still completed: %v", err)
	}
	if pending, err := configtx.PendingJournals(ctx, r, root); err != nil || len(pending) != 0 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	// The restore's recovery ref now has no completed journal; doctor names
	// the quarantine that holds it.
	orphans, err := restore.OrphanRefs(ctx, r, w)
	if err != nil || len(orphans) != 1 || orphans[0].ID != restoreID || orphans[0].Quarantine != result.Quarantine {
		t.Fatalf("orphan refs: %+v %v", orphans, err)
	}
}

// TestAbandonRerunAfterJournalQuarantined: an abandon interrupted after it
// moved a mismatched completed journal into the quarantine, but before it
// marked the migration abandoned, must finish on a rerun instead of failing
// on the journal's old, now empty, place.
func TestAbandonRerunAfterJournalQuarantined(t *testing.T) {
	r, w, root, key, restoreID, _ := restoredMigrationFixture(t)
	ctx := context.Background()
	id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
		if point == "policy-published" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault missed")
	}
	completed := filepath.Join(w.Common, "gitlocal", "transactions", "completed", "restore-"+restoreID+".json")
	data, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(completed, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(key, key+".away"); err != nil {
		t.Fatal(err)
	}
	// Simulate the interrupted abandon: the journal is quarantined, nothing else happened.
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := configtx.NewQuarantine(meta, id, "encrypt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.QuarantineJournal(meta, "transactions/completed/restore-"+restoreID+".json", "test"); err != nil {
		t.Fatal(err)
	}
	meta.Close()
	if _, err := AbandonMigration(ctx, r, root, id); err != nil {
		t.Fatalf("abandon rerun after the journal was quarantined: %v", err)
	}
	if pending, err := configtx.PendingJournals(ctx, r, root); err != nil || len(pending) != 0 {
		t.Fatalf("pending after rerun: %+v %v", pending, err)
	}
}

// A quarantined seed journal takes the file list kept beside it into the
// quarantine: left in transactions/seed-files it would be an orphan naming
// the plaintext history, carried into backups.
func TestAbandonQuarantinesSeedFileListWithItsJournal(t *testing.T) {
	r, w, root, key, _, seedID := seededMigrationFixture(t)
	state := filepath.Join(w.Common, "gitlocal")
	testutil.SeedFilesBeside(t, state, seedID, true)
	ctx := context.Background()
	id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
		if point == "policy-published" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault missed")
	}
	completed := filepath.Join(state, "transactions", "completed", "seed-"+seedID+".json")
	data, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(completed, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := AbandonMigration(ctx, r, root, id)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	list := filepath.Join(state, "transactions", "seed-files", seedID+".json")
	if _, err := os.Stat(list); !os.IsNotExist(err) {
		t.Fatalf("seed file list left beside a quarantined journal: %v", err)
	}
	var kept string
	for _, entry := range result.Quarantined {
		if entry.Kind == "seed-files" && entry.Source == "transactions/seed-files/"+seedID+".json" {
			kept = filepath.Join(result.Quarantine, filepath.FromSlash(entry.File))
		}
	}
	if kept == "" {
		t.Fatalf("abandon result: %+v", result.Quarantined)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("quarantined seed file list: %v", err)
	}
}
