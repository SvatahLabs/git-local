package encryption

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

func TestAbandonMigrationAfterPublicationQuarantinesStores(t *testing.T) {
	r, w, root, key, _ := migrationFixture(t)
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
	// Without the key, resume can't verify the encrypted store and rollback is
	// refused after publication.
	hidden := key + ".away"
	if err := os.Rename(key, hidden); err != nil {
		t.Fatal(err)
	}
	if err := RecoverMigration(ctx, r, root, id, "resume"); err == nil {
		t.Fatal("resume succeeded without the key")
	}
	if err := RecoverMigration(ctx, r, root, id, "rollback"); err == nil {
		t.Fatal("rollback accepted after publication")
	}
	pending, err := configtx.PendingJournals(ctx, r, root)
	if err != nil || len(pending) != 1 || pending[0].Kind != "encrypt" || !pending[0].Published {
		t.Fatalf("pending journals: %+v %v", pending, err)
	}
	result, err := AbandonMigration(ctx, r, root, id)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	moved := map[string]bool{}
	for _, entry := range result.Quarantined {
		moved[entry.File] = true
		if _, err := os.Stat(filepath.Join(result.Quarantine, entry.File)); err != nil {
			t.Fatalf("quarantined %s missing: %v", entry.File, err)
		}
	}
	if !moved[backupName(id)] || len(result.Warnings) == 0 {
		t.Fatalf("plaintext store not quarantined with a warning: %+v", result)
	}
	for _, name := range []string{backupName(id), stageName(id), recordsName(id), journalName(id)} {
		if _, err := os.Lstat(filepath.Join(w.Common, "gitlocal", name)); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(w.Common, "gitlocal", completedMigrationName(id)))
	var record struct {
		Outcome string `json:"outcome"`
	}
	if err == nil {
		err = json.Unmarshal(data, &record)
	}
	if err != nil || record.Outcome != configtx.OutcomeAbandoned {
		t.Fatalf("completed record: %q %v", record.Outcome, err)
	}
	if _, err := configtx.LoadGuardSnapshot(ctx, r, root); err != nil {
		t.Fatalf("guards still blocked: %v", err)
	}
	if err := os.Rename(hidden, key); err != nil {
		t.Fatal(err)
	}
	if count, err := history.AuditWorkspaceLocked(ctx, r, w, w.WorkspaceID); err != nil || count != 2 {
		t.Fatalf("encrypted history after abandon: %d %v", count, err)
	}
	if _, err := history.Commit(ctx, r, root, "after abandon"); err != nil {
		t.Fatalf("snapshot after abandon: %v", err)
	}
	if _, err := AbandonMigration(ctx, r, root, id); err != nil {
		t.Fatalf("abandon isn't idempotent: %v", err)
	}
}

func TestAbandonMigrationRefusesEncryptedStoreWithoutPolicy(t *testing.T) {
	r, _, root, key, _ := migrationFixture(t)
	ctx := context.Background()
	id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
		if point == "encrypted-store-published" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault missed")
	}
	if _, err := AbandonMigration(ctx, r, root, id); err == nil {
		t.Fatal("abandon left an encrypted store in service without its policy")
	}
	if err := RecoverMigration(ctx, r, root, id, "rollback"); err != nil {
		t.Fatalf("rollback after refused abandon: %v", err)
	}
}

func TestAbandonMigrationAfterPublicationAppliesRecordUpdates(t *testing.T) {
	r, w, root, key, restoreID, preimage := restoredMigrationFixture(t)
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
	if _, err := workspace.Init(ctx, r, root); err == nil || !strings.Contains(err.Error(), "git local recover "+id+" --abandon") {
		t.Fatalf("pending migration error doesn't name abandon: %v", err)
	}
	hidden := key + ".away"
	if err := os.Rename(key, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := AbandonMigration(ctx, r, root, id); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if err := os.Rename(hidden, key); err != nil {
		t.Fatal(err)
	}
	// The completed restore journal must describe the encrypted history now
	// in service, not the quarantined plaintext one.
	checkConvertedRestore(t, ctx, r, w, root, restoreID, preimage)
}
