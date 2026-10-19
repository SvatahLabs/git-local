package configtx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// A seeded file's stage is published by a rename into its own directory,
// whose directory sync covers the stage and the file alike, so the stage's
// directory isn't synced before the rename (GL-120). A crash before the
// rename can then lose the stage's name: resume writes it again from the
// journal, and rollback has nothing of it to remove.
func TestSeedRecoversStageLostBeforePublication(t *testing.T) {
	ctx := context.Background()
	for _, at := range []int{0, 1} {
		for _, rollback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/rollback=%v", at, rollback), func(t *testing.T) {
				t.Parallel()
				repo, r, main := fixture(t)
				repo.Commit()
				linked := filepath.Join(t.TempDir(), "linked")
				repo.Git("worktree", "add", "-q", "-b", "seed", linked)
				w, err := workspace.Init(ctx, r, linked)
				if err != nil {
					t.Fatal(err)
				}
				contents := map[string]string{"notes/a": "seeded-a", "notes/sub/b": "seeded-b"}
				files := []SeedFile{}
				for _, name := range []string{"notes/a", "notes/sub/b"} {
					out, err := r.History(ctx, w.Store, strings.NewReader(contents[name]), "hash-object", "-w", "--stdin", "--no-filters")
					if err != nil {
						t.Fatal(err)
					}
					files = append(files, SeedFile{Path: name, Mode: "100644", Blob: strings.TrimSpace(string(out))})
				}
				planner := func(workspace.Context) (SeedPlan, error) {
					state := selection.Empty()
					state.Generation = 1
					state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
					return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: files[0].Blob, Selection: state, Files: append([]SeedFile{}, files...)}, nil
				}
				var stage string
				result, err := seed(ctx, r, linked, planner, func(name string) error {
					if name == fmt.Sprintf("seed-publish-%d", at) {
						matches, _ := filepath.Glob(filepath.Join(linked, filepath.Dir(filepath.FromSlash(files[at].Path)), ".gitlocal-tmp-*"))
						if len(matches) != 1 {
							t.Errorf("stages before publication: %v", matches)
						} else {
							stage = matches[0]
						}
						return fmt.Errorf("injected interruption at %s", name)
					}
					return nil
				})
				if err == nil || stage == "" {
					t.Fatalf("seed wasn't stopped before publishing file %d: %v", at, err)
				}
				// The stage's name was lost in the crash.
				if err := os.Remove(stage); err != nil {
					t.Fatal(err)
				}
				if _, err := RecoverSeed(ctx, r, linked, result.TransactionID, rollback); err != nil {
					t.Fatalf("recover (rollback %v): %v", rollback, err)
				}
				if pending, err := Pending(ctx, r, linked); err != nil || len(pending) != 0 {
					t.Fatalf("pending after recovery: %v %v", pending, err)
				}
				for _, file := range files {
					b, err := os.ReadFile(filepath.Join(linked, filepath.FromSlash(file.Path)))
					if rollback {
						if !os.IsNotExist(err) {
							t.Fatalf("rollback left %s: %q %v", file.Path, b, err)
						}
					} else if err != nil || string(b) != contents[file.Path] {
						t.Fatalf("resume: %s: %q %v", file.Path, b, err)
					}
				}
				if rollback {
					if _, err := os.Lstat(filepath.Join(linked, "notes")); !os.IsNotExist(err) {
						t.Fatalf("rollback left the seed's directory: %v", err)
					}
				}
				for _, dir := range []string{"notes", "notes/sub"} {
					if stages, _ := filepath.Glob(filepath.Join(linked, filepath.FromSlash(dir), ".gitlocal-tmp-*")); len(stages) != 0 {
						t.Fatalf("stages left: %v", stages)
					}
				}
			})
		}
	}
}
