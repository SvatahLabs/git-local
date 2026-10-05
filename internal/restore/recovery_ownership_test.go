package restore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/storage"
)

func TestRecoveryAfterMainCommitIndexAndBranchChange(t *testing.T) {
	for _, mode := range []string{"resume", "rollback", "owned-resume", "owned-rollback"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			repo, r, _ := fixture(t)
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
			// Ordinary Git work after the interruption: a branch switch, an
			// unrelated commit and a staged change.
			repo.Git("checkout", "-q", "-b", "feature")
			write(t, root, "unrelated.txt", "mainline")
			repo.Git("add", "unrelated.txt")
			repo.Git("commit", "-qm", "unrelated")
			write(t, root, "staged.txt", "staged")
			repo.Git("add", "staged.txt")
			owned := mode == "owned-resume" || mode == "owned-rollback"
			if owned {
				repo.Git("add", "-f", "notes/a")
			}
			rollback := mode == "rollback" || mode == "owned-rollback"
			_, err = Recover(ctx, r, root, result.TransactionID, rollback)
			if owned {
				if !errors.Is(err, storage.ErrConflict) {
					t.Fatalf("recovery accepted a mainline-owned restore path: %v", err)
				}
				data, e := os.ReadFile(filepath.Join(root, "notes/a"))
				if e != nil || string(data) != "baseline" {
					t.Fatalf("refused recovery changed the file: %q %v", data, e)
				}
				return
			}
			if err != nil {
				t.Fatalf("recovery after unrelated Git work: %v", err)
			}
			want := "source"
			if rollback {
				want = "baseline"
			}
			data, err := os.ReadFile(filepath.Join(root, "notes/a"))
			if err != nil || string(data) != want {
				t.Fatalf("restored file: %q %v", data, err)
			}
		})
	}
}
