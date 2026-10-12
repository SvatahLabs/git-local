package encryption

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/history"
	"gitlocal/internal/storage"
)

// GL-108: with the key's directory gone (an ejected drive), a policy
// correction interrupted before publication still rolls back; resuming it, a
// snapshot and a new correction need the key and name encrypt rebind-key.
func TestPolicyCorrectionRollsBackWithoutKeyDirectory(t *testing.T) {
	t.Parallel()
	f := legacyPolicyFixture(t)
	ctx := context.Background()
	id, err := f.correct(func(at string) error {
		if at == "encrypted-store-published" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("interruption: %v", err)
	}
	drive := filepath.Dir(f.key)
	ejected := drive + "-ejected"
	if err := os.Rename(drive, ejected); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ejected) })
	rebind := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "git local encrypt rebind-key") || !strings.Contains(err.Error(), f.key) {
			t.Fatalf("%s without the key's directory: %v", what, err)
		}
	}
	rebind("resume", RecoverMigration(ctx, f.r, f.root, id, "resume"))
	if err := RecoverMigration(ctx, f.r, f.root, id, "rollback"); err != nil {
		t.Fatalf("rollback without the key's directory: %v", err)
	}
	for _, blob := range f.plainBlobs {
		if _, err := f.r.History(ctx, f.w.Store, nil, "cat-file", "-t", blob); err != nil {
			t.Fatalf("rollback lost the original history: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.w.Common, "gitlocal", journalName(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left its journal: %v", err)
	}
	_, err = history.Commit(ctx, f.r, f.root, "needs the key")
	rebind("snapshot", err)
	// The key named on the command line is missing: rebinding is no remedy.
	_, err = Migrate(ctx, f.r, f.root, f.key, []string{".env.local", "config/secrets"})
	if err == nil || !strings.Contains(err.Error(), f.key+" doesn't exist; check the path, or reconnect") || strings.Contains(err.Error(), "rebind-key") {
		t.Fatalf("correction without the key's directory: %v", err)
	}
	if err := os.Rename(ejected, drive); err != nil {
		t.Fatal(err)
	}
	if got := f.policy(t).Sensitive; len(got) != 2 || got[1].Kind != "file" {
		t.Fatalf("rollback didn't keep the old policy: %+v", got)
	}
	if _, err := Migrate(ctx, f.r, f.root, f.key, []string{".env.local", "config/secrets"}); err != nil {
		t.Fatalf("correction with the key back: %v", err)
	}
	f.checkCorrected(t)
}
