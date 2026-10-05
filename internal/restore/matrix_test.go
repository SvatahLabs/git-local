package restore

import (
	"errors"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
)

func TestEveryRestoreFaultIsResolvable(t *testing.T) {
	setup := func(t *testing.T) (string, *gitexec.Runner, func(fault) (Result, error)) {
		repo, r, _ := fixture(t)
		root := repo.Root
		write(t, root, "notes/a", "source")
		write(t, root, "notes/z/new", "new")
		track(t, r, root, true, "notes")
		source := save(t, r, root)
		write(t, root, "notes/a", "baseline")
		remove(t, root, "notes/z")
		write(t, root, "notes/b", "delete")
		save(t, r, root)
		return root, r, func(f fault) (Result, error) {
			return execute(ctx, r, root, "", source, []string{"notes"}, false, f)
		}
	}
	_, _, run := setup(t)
	var points []string
	if _, err := run(func(name string) error { points = append(points, name); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		for _, rollback := range []bool{false, true} {
			t.Run(point+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				t.Parallel()
				root, r, run := setup(t)
				result, err := run(func(name string) error {
					if name == point {
						return errors.New("injected interruption")
					}
					return nil
				})
				if err == nil {
					t.Fatalf("fault %s missed", point)
				}
				pending, err := configtx.Pending(ctx, r, root)
				if err != nil {
					t.Fatal(err)
				}
				if len(pending) == 0 {
					return
				}
				how := "resume"
				if rollback {
					how = "rollback"
				}
				if _, err := Recover(ctx, r, root, result.TransactionID, rollback); err != nil {
					how = "abandon"
					if _, err := Abandon(ctx, r, root, result.TransactionID); err != nil {
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
