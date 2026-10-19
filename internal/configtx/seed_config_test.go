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

// seedConfigReads seeds n files, each its own file rule among rules file
// rules, and returns how often the seed read and validated every
// workspace's configuration.
func seedConfigReads(t *testing.T, n, rules int) int {
	t.Helper()
	ctx := context.Background()
	repo, r, main := fixture(t)
	repo.Commit()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(out))
	planner := func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		files := []SeedFile{}
		for i := 0; i < rules; i++ {
			name := fmt.Sprintf("notes/file-%04d", i)
			state.Include = append(state.Include, selection.Rule{Path: name, Kind: "file"})
			if i < n {
				files = append(files, SeedFile{Path: name, Mode: "100644", Blob: blob})
			}
		}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: blob, Selection: state, Files: files}, nil
	}
	reads := 0
	sourcesRead = func() { reads++ }
	defer func() { sourcesRead = func() {} }()
	if _, err := Seed(ctx, r, linked, planner); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		name := filepath.Join(linked, "notes", fmt.Sprintf("file-%04d", i))
		if b, err := os.ReadFile(name); err != nil || string(b) != "seeded" {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	return reads
}

// Before each seeded file a seed stats the configuration files and reads and
// validates them again only when one changed, so its configuration work
// doesn't grow with its files times the selection's rules. Before, each file
// read every workspace's records and canonicalized every selection (K² for K
// rules). Not parallel: it replaces sourcesRead.
func TestSeedRereadsConfigurationOnlyWhenItChanged(t *testing.T) {
	const rules = 500
	small, large := seedConfigReads(t, 5, rules), seedConfigReads(t, 40, rules)
	t.Logf("configuration reads seeding with %d rules: 5 files %d, 40 files %d", rules, small, large)
	if large > small+2 {
		t.Fatalf("35 more seeded files read the configuration %d more times", large-small)
	}
}

// An edit to a registration or selection during a seed still stops it at the
// next file, as it did when each file read the configuration again.
func TestSeedStopsWhenConfigurationChangesBetweenFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, r, main := fixture(t)
	repo.Commit()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(out))
	planner := func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: blob, Selection: state, Files: []SeedFile{{Path: "notes/a", Mode: "100644", Blob: blob}, {Path: "notes/b", Mode: "100644", Blob: blob}}}, nil
	}
	selectionFile := filepath.Join(main.Common, "gitlocal", "workspaces", main.WorkspaceID, "selection.json")
	_, err = seed(ctx, r, linked, planner, func(at string) error {
		if at != "seed-file-0" {
			return nil
		}
		// Same size, so only the stat's times and identity change.
		b, e := os.ReadFile(selectionFile)
		if e != nil {
			return e
		}
		b = []byte(strings.Replace(string(b), `"generation": 0`, `"generation": 9`, 1))
		return os.WriteFile(selectionFile, b, 0600)
	})
	if err == nil || !strings.Contains(err.Error(), "another workspace selection changed") {
		t.Fatalf("seed accepted an edited selection: %v", err)
	}
	if _, e := os.Lstat(filepath.Join(linked, "notes", "b")); !os.IsNotExist(e) {
		t.Fatalf("seed wrote notes/b after the selection changed: %v", e)
	}
}
