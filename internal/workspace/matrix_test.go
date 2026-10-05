package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

// Initialization resumes by running init again from its checkout; after the
// checkout is removed, the journal is abandoned from another one.
func TestEveryInitializationFaultIsResolvable(t *testing.T) {
	ctx := context.Background()
	record := testutil.New(t)
	record.Commit()
	var points []string
	if _, err := initialize(ctx, runner(t, record), record.Root, func(name string) error { points = append(points, name); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		for _, removed := range []bool{false, true} {
			t.Run(point+map[bool]string{false: "/resume", true: "/abandon"}[removed], func(t *testing.T) {
				t.Parallel()
				r := testutil.New(t)
				r.Commit()
				g := runner(t, r)
				linked := filepath.Join(t.TempDir(), "linked")
				r.Git("worktree", "add", "-q", "-b", "linked", linked)
				if _, err := initialize(ctx, g, linked, func(name string) error {
					if name == point {
						return errors.New("injected interruption")
					}
					return nil
				}); err == nil {
					t.Fatalf("fault %s missed", point)
				}
				journal := filepath.Join(r.Root, ".git", "gitlocal", "transactions", "init.json")
				if _, err := os.Lstat(journal); errors.Is(err, os.ErrNotExist) {
					return
				}
				var id string
				if removed {
					var j initJournal
					data, err := os.ReadFile(journal)
					if err != nil {
						t.Fatal(err)
					}
					if err := jsonUnmarshal(data, &j); err != nil {
						t.Fatal(err)
					}
					id = j.Identity.WorkspaceID
					r.Git("worktree", "remove", "--force", linked)
					if _, err := AbandonInit(ctx, g, r.Root, id); err != nil {
						t.Fatalf("abandon: %v", err)
					}
				} else if _, err := Init(ctx, g, linked); err != nil {
					t.Fatalf("resume: %v", err)
				}
				if _, err := os.Lstat(journal); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("journal remains: %v", err)
				}
				w, err := Init(ctx, g, r.Root)
				if err != nil {
					t.Fatalf("init in the main checkout afterwards: %v", err)
				}
				if removed && w.WorkspaceID == id {
					t.Fatal("abandoned identity reused")
				}
			})
		}
	}
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
