package configtx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// largeSeedPlanner plans n files under long directory names, so that their
// records alone exceed the 1 MiB journal limit.
func largeSeedPlanner(main *workspace.Context, blob string, n int) (SeedPlanner, []string) {
	dir := "notes/" + strings.Repeat("d", 200) + "/" + strings.Repeat("e", 200) + "/" + strings.Repeat("f", 200) + "/" + strings.Repeat("g", 200)
	paths := make([]string, n)
	files := make([]SeedFile, n)
	for i := range files {
		paths[i] = fmt.Sprintf("%s/file-%04d", dir, i)
		files[i] = SeedFile{Path: paths[i], Mode: "100644", Blob: blob}
	}
	return func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: blob, Selection: state, Files: append([]SeedFile{}, files...)}, nil
	}, paths
}

// A seed whose file list exceeds the journal limit keeps the list in a
// sidecar file the journal names by digest: it seeds, can be diagnosed, and
// resumes after an interruption.
func TestSeedOfManyFilesKeepsFileListOutOfJournal(t *testing.T) {
	t.Parallel()
	repo, r, main := fixture(t)
	repo.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	planner, paths := largeSeedPlanner(main, strings.TrimSpace(string(blob)), 650)
	stop := fmt.Sprintf("seed-file-%d", len(paths)/2)
	result, err := seed(ctx, r, linked, planner, func(name string) error {
		if name == stop {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil || result.TransactionID == "" {
		t.Fatalf("seed did not stop at %s: %+v %v", stop, result, err)
	}
	if !strings.Contains(err.Error(), "injected interruption") {
		t.Fatalf("seed failed before its interruption: %v", err)
	}
	journal, err := os.ReadFile(filepath.Join(w.Common, "gitlocal", seedPath(result.TransactionID)))
	if err != nil || len(journal) > 1<<20 || strings.Contains(string(journal), paths[0]) {
		t.Fatalf("seed journal holds the file list (%d bytes): %v", len(journal), err)
	}
	if err := ReadDiagnostic(ctx, r, linked, func(d DiagnosticState) error {
		if d.JournalErr != nil || len(d.Journals) != 1 || d.Journals[0].Err != nil {
			t.Fatalf("seed diagnosis: %+v %v", d.Journals, d.JournalErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverSeed(ctx, r, linked, result.TransactionID, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if data, err := os.ReadFile(filepath.Join(linked, filepath.FromSlash(p))); err != nil || string(data) != "seeded" {
			t.Fatalf("%s: %q %v", p, data, err)
		}
	}
	if pending, err := Pending(ctx, r, linked); err != nil || len(pending) != 0 {
		t.Fatalf("pending after recovery: %v %v", pending, err)
	}
	if _, err := RecoverSeed(ctx, r, linked, result.TransactionID, false); err != nil {
		t.Fatalf("completed seed with a sidecar file list: %v", err)
	}
	// Every reader of the transactions directory accepts the file lists.
	if journals, err := PendingJournals(ctx, r, linked); err != nil || len(journals) != 0 {
		t.Fatalf("pending journals beside a seed file list: %v %v", journals, err)
	}
	if _, err := LoadGuardSnapshot(ctx, r, linked); err != nil {
		t.Fatalf("guards beside a seed file list: %v", err)
	}
	if _, err := workspace.Init(ctx, r, linked); err != nil {
		t.Fatalf("init beside a seed file list: %v", err)
	}
	if err := ReadDiagnostic(ctx, r, linked, func(d DiagnosticState) error {
		if d.JournalErr != nil || len(d.Journals) != 1 || d.Journals[0].Err != nil || !d.Journals[0].Completed {
			t.Fatalf("completed seed diagnosis: %+v %v", d.Journals, d.JournalErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A file list beyond even the sidecar limit is refused before anything is
// written, naming the cause and the workaround. Not parallel: it lowers
// seedFilesLimit.
func TestSeedRefusesFileListBeyondLimitBeforeWriting(t *testing.T) {
	restore := seedFilesLimit
	seedFilesLimit = 64 << 10
	t.Cleanup(func() { seedFilesLimit = restore })
	repo, r, main := fixture(t)
	repo.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	planner, _ := largeSeedPlanner(main, strings.Repeat("a", 40), 650)
	_, err = Seed(ctx, r, linked, planner)
	if err == nil || !strings.Contains(err.Error(), "--paths") {
		t.Fatalf("oversized seed accepted or unexplained: %v", err)
	}
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	for _, dir := range []string{"transactions", "transactions/seed-files"} {
		if entries, err := meta.ReadDir(dir); err == nil && len(entries) != 0 {
			for _, e := range entries {
				if e.Name() != "seed-files" && e.Name() != "completed" {
					t.Fatalf("refused seed wrote %s/%s", dir, e.Name())
				}
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(linked, "notes")); !os.IsNotExist(err) || len(load(t, w).Include) != 0 {
		t.Fatalf("refused seed changed the worktree: %v", err)
	}
}
