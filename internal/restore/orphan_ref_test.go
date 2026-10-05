package restore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/history"
	"gitlocal/internal/secureblob"
)

func TestRestoreStoppedBeforeJournalLeavesNoRecoveryRef(t *testing.T) {
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
			result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
				if at == cut {
					return errors.New("injected interruption")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault missed")
			}
			if strings.Contains(err.Error(), "git local recover") || result.RecoverySnapshot != "" {
				t.Fatalf("stopped restore points at a transaction without a journal: %v %+v", err, result)
			}
			refs, err := r.History(ctx, w.Store, nil, "show-ref")
			if err != nil || strings.Contains(string(refs), "refs/gitlocal/recovery/") {
				t.Fatalf("orphan recovery ref: %q %v", refs, err)
			}
			if pending, err := configtx.Pending(ctx, r, root); err != nil || len(pending) != 0 {
				t.Fatalf("pending: %v %v", pending, err)
			}
			// Encryption migration copies only known refs into a new store.
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
