package encryption

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/history"
)

func TestMigrationResumeRemovesPartlyDeletedPlaintextStore(t *testing.T) {
	r, w, root, key, _ := migrationFixture(t)
	ctx := context.Background()
	id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
		if point == "record-updates" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault missed")
	}
	// Removal of the replaced plaintext store stopped after its config and
	// refs were gone.
	backup := filepath.Join(w.Common, "gitlocal", backupName(id))
	for _, name := range []string{"config", "refs", "HEAD"} {
		if err := os.RemoveAll(filepath.Join(backup, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecoverMigration(ctx, r, root, id, "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("plaintext store remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(w.Common, "gitlocal", journalName(id))); !os.IsNotExist(err) {
		t.Fatalf("journal remains: %v", err)
	}
	if count, err := history.AuditWorkspaceLocked(ctx, r, w, w.WorkspaceID); err != nil || count != 2 {
		t.Fatalf("encrypted history after resume: %d %v", count, err)
	}
}
