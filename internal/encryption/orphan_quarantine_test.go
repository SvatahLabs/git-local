package encryption

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/doctor"
	"github.com/SvatahLabs/git-local/internal/restore"
	"github.com/SvatahLabs/git-local/internal/storage"
)

// After an encryption abandon quarantined a restore's completed journal, the
// restore's recovery ref is all that keeps its previous file versions.
// Abandoning that ref needs --discard-recovery, and doctor names what the ref
// blocks, including restoring an encrypted backup.
func TestAbandonOrphanRefOfQuarantinedJournalNeedsDiscardRecovery(t *testing.T) {
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
	completed := w.Common + "/gitlocal/transactions/completed/restore-" + restoreID + ".json"
	data, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(completed, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AbandonMigration(ctx, r, root, id); err != nil {
		t.Fatal(err)
	}
	orphans, err := restore.OrphanRefs(ctx, r, w)
	if err != nil || len(orphans) != 1 || orphans[0].Quarantine == "" {
		t.Fatalf("orphan refs: %+v %v", orphans, err)
	}
	report, err := doctor.Run(ctx, r, root)
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, finding := range report.Findings {
		named = named || finding.Check == "recovery refs" && strings.Contains(finding.Message, "encrypted backup") && strings.Contains(finding.Action, "--abandon --discard-recovery")
	}
	if !named {
		t.Fatalf("doctor doesn't name the backup consequence and flag: %+v", report.Findings)
	}
	if _, err := restore.AbandonOrphanRef(ctx, r, root, restoreID, false); !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "--discard-recovery") {
		t.Fatalf("abandon without --discard-recovery: %v", err)
	}
	if kept, err := restore.OrphanRefs(ctx, r, w); err != nil || len(kept) != 1 {
		t.Fatalf("refused abandon deleted the ref: %+v %v", kept, err)
	}
	result, err := restore.AbandonOrphanRef(ctx, r, root, restoreID, true)
	if err != nil || len(result.Removed) != 1 || result.Removed[0] != orphans[0].Ref {
		t.Fatalf("abandon with --discard-recovery: %+v %v", result, err)
	}
	if left, err := restore.OrphanRefs(ctx, r, w); err != nil || len(left) != 0 {
		t.Fatalf("ref kept after --discard-recovery: %+v %v", left, err)
	}
}
