package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/history"
	"gitlocal/internal/secureblob"
)

// TestRestoreKilledBeforeJournalIsAbandonable kills a restore subprocess after
// it published its recovery ref but before it wrote its journal. No cleanup
// runs, so the ref stays. The ref is listed, recover --abandon removes it,
// encryption migration then proceeds, and no file changed.
func TestRestoreKilledBeforeJournalIsAbandonable(t *testing.T) {
	for _, cut := range []string{"recovery-ref", "before-journal"} {
		t.Run(cut, func(t *testing.T) {
			t.Parallel()
			repo, r, w := fixture(t)
			root := repo.Root
			write(t, root, "notes/a", "source")
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			write(t, root, "notes/a", "baseline")
			save(t, r, root)
			crash(t, repo.RunnerEnv(), root, source, "", cut)
			if pending, err := configtx.Pending(ctx, r, root); err != nil || len(pending) != 0 {
				t.Fatalf("pending: %v %v", pending, err)
			}
			orphans, err := OrphanRefs(ctx, r, *w)
			if err != nil || len(orphans) != 1 || orphans[0].WorkspaceID != w.WorkspaceID || !strings.HasSuffix(orphans[0].Ref, "/"+orphans[0].ID) {
				t.Fatalf("orphan refs: %+v %v", orphans, err)
			}
			result, err := AbandonOrphanRef(ctx, r, root, orphans[0].ID, false)
			if err != nil || len(result.Removed) != 1 || result.Removed[0] != orphans[0].Ref {
				t.Fatalf("abandon: %+v %v", result, err)
			}
			if again, err := OrphanRefs(ctx, r, *w); err != nil || len(again) != 0 {
				t.Fatalf("orphan refs after abandon: %+v %v", again, err)
			}
			if _, err := AbandonOrphanRef(ctx, r, root, orphans[0].ID, false); !os.IsNotExist(err) {
				t.Fatalf("second abandon: %v", err)
			}
			destination := filepath.Join(t.TempDir(), "migrated.git")
			if err := r.InitBare(ctx, destination); err != nil {
				t.Fatal(err)
			}
			if _, err := history.RewritePlaintextStore(ctx, r, *w, destination, secureblob.Policy{}, make([]byte, 32)); err != nil {
				t.Fatalf("migration refused the store: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(root, "notes/a")); err != nil || string(data) != "baseline" {
				t.Fatalf("file changed: %q %v", data, err)
			}
			if _, err := Execute(ctx, r, root, "", source, []string{"notes"}, false); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}
