package encryption

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
)

func TestEveryEncryptionMigrationFaultIsResolvable(t *testing.T) {
	ctx := context.Background()
	r, _, root, key, _ := migrationFixture(t)
	var points []string
	if _, err := migrate(ctx, r, root, key, []string{".env.local"}, func(name string) error { points = append(points, name); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		for _, rollback := range []bool{false, true} {
			t.Run(point+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				t.Parallel()
				r, w, root, key, _ := migrationFixture(t)
				id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(name string) error {
					if name == point {
						return errors.New("injected interruption")
					}
					return nil
				})
				if err == nil {
					t.Fatalf("fault %s missed", point)
				}
				if _, err := os.Lstat(filepath.Join(w.Common, "gitlocal", journalName(id))); errors.Is(err, os.ErrNotExist) {
					return
				}
				how := "resume"
				if rollback {
					how = "rollback"
				}
				if err := RecoverMigration(ctx, r, root, id, how); err != nil {
					how = "abandon"
					if _, err := AbandonMigration(ctx, r, root, id); err != nil {
						t.Fatalf("recovery and abandon both failed: %v", err)
					}
				}
				t.Logf("resolved by %s", how)
				if pending, err := configtx.Pending(ctx, r, root); err != nil || len(pending) != 0 {
					t.Fatalf("pending after %s: %v %v", how, pending, err)
				}
				if _, err := configtx.LoadGuardSnapshot(ctx, r, root); err != nil {
					t.Fatalf("guards blocked after %s: %v", how, err)
				}
			})
		}
	}
}
