package configtx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/secureblob"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// sidecarSeed seeds notes/a, notes/b and notes/c into a linked worktree,
// stopping at stop (or finishing when stop is empty), and moves the journal's
// file list beside it as a large seed keeps it.
func sidecarSeed(t *testing.T, stop string) (*gitexec.Runner, *workspace.Context, string, string) {
	t.Helper()
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
	id := strings.TrimSpace(string(blob))
	planner := func(workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		var files []SeedFile
		for _, name := range []string{"notes/a", "notes/b", "notes/c"} {
			files = append(files, SeedFile{Path: name, Mode: "100644", Blob: id})
		}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: id, Selection: state, Files: files}, nil
	}
	result, err := seed(ctx, r, linked, planner, func(name string) error {
		if name == stop {
			return errors.New("injected interruption")
		}
		return nil
	})
	if stop == "" && err != nil || stop != "" && (err == nil || !strings.Contains(err.Error(), "injected interruption")) {
		t.Fatalf("seed stopping at %q: %+v %v", stop, result, err)
	}
	testutil.SeedFilesBeside(t, filepath.Join(w.Common, "gitlocal"), result.TransactionID, stop == "")
	return r, w, linked, result.TransactionID
}

func seedListPath(w *workspace.Context, id string) string {
	return filepath.Join(w.Common, "gitlocal", "transactions", "seed-files", id+".json")
}

// GL-119: a pending seed whose file list is missing or damaged can't resume
// or roll back; each of them, recover --list and doctor's journal check name
// the list and the remedy, and abandon works without the list, moving the
// journal into the quarantine.
func TestLostSeedListNamesListAndAbandonQuarantinesJournal(t *testing.T) {
	for _, kind := range []string{"missing", "damaged"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r, w, linked, id := sidecarSeed(t, "seed-file-0")
			list := seedListPath(w, id)
			if kind == "missing" {
				if err := os.Remove(list); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(list, []byte("{\"files\": []}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			named := func(what string, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "seed-files/"+id+".json") || !strings.Contains(err.Error(), "recover "+id+" --abandon") || !errors.Is(err, storage.ErrConflict) {
					t.Fatalf("%s with a %s seed list: %v", what, kind, err)
				}
			}
			for _, rollback := range []bool{false, true} {
				_, err := RecoverSeed(ctx, r, linked, id, rollback)
				named("recover", err)
			}
			pending, err := PendingJournals(ctx, r, linked)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending journals: %+v %v", pending, err)
			}
			named("recover --list", pending[0].Err)
			if next, _ := pending[0].Next(); !strings.Contains(next, "recover "+id+" --abandon") {
				t.Fatalf("recover --list next step: %q", next)
			}
			if err := ReadDiagnostic(ctx, r, linked, func(d DiagnosticState) error {
				if len(d.Journals) != 1 {
					t.Fatalf("diagnosis: %+v", d.Journals)
				}
				named("doctor", d.Journals[0].Err)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Abandon works from any worktree; its warning names the one where
			// doctor sees the seed's staging files.
			from := linked
			if kind == "missing" {
				from = filepath.Dir(w.Common)
			}
			result, err := Abandon(ctx, r, from, id)
			if err != nil || result.Quarantine == "" {
				t.Fatalf("abandon without the seed list: %+v %v", result, err)
			}
			if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "git local doctor run in "+w.Root+" lists") {
				t.Errorf("abandon from %s warns: %q", from, result.Warnings)
			}
			if _, err := os.Stat(filepath.Join(result.Quarantine, "journals", "seed-"+id+".json")); err != nil {
				t.Fatalf("journal not quarantined: %v", err)
			}
			if _, err := os.Lstat(list); !os.IsNotExist(err) {
				t.Fatalf("damaged list left in place: %v", err)
			}
			if pending, err := Pending(ctx, r, linked); err != nil || len(pending) != 0 {
				t.Fatalf("pending after abandon: %v %v", pending, err)
			}
			if data, err := os.ReadFile(filepath.Join(linked, "notes", "a")); err != nil || string(data) != "seeded" {
				t.Fatalf("abandon changed a seeded file: %q %v", data, err)
			}
			// recover <id> --abandon run again finds the quarantined journal.
			if kind, err := JournalKind(ctx, r, linked, id); err != nil || kind != "seed" {
				t.Fatalf("journal kind after the abandon: %q %v", kind, err)
			}
			if again, err := Abandon(ctx, r, linked, id); err != nil || again.Kind != "seed" {
				t.Fatalf("abandon again: %+v %v", again, err)
			}
		})
	}
}

// GL-119: a completed seed whose file list is lost blocks encryption
// migration and backups; abandon quarantines its journal, after which doctor
// finds every journal readable.
func TestLostListOfCompletedSeedIsAbandoned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, w, linked, id := sidecarSeed(t, "")
	if err := os.Remove(seedListPath(w, id)); err != nil {
		t.Fatal(err)
	}
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	if err := CheckSeedList(meta, seedCompletedPath(id)); err == nil || !strings.Contains(err.Error(), "--abandon") {
		t.Fatalf("lost list of a completed seed: %v", err)
	}
	result, err := Abandon(ctx, r, linked, id)
	if err != nil || result.Quarantine == "" {
		t.Fatalf("abandon a completed seed without its list: %+v %v", result, err)
	}
	// A completed seed left no staging files to warn about.
	if len(result.Warnings) != 0 {
		t.Fatalf("abandon of a completed seed warns: %q", result.Warnings)
	}
	if err := ReadDiagnostic(ctx, r, linked, func(d DiagnosticState) error {
		for _, j := range d.Journals {
			if j.Err != nil {
				t.Fatalf("journal %s after abandon: %v", j.ID, j.Err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(linked, "notes", "c")); err != nil || string(data) != "seeded" {
		t.Fatalf("abandon changed a seeded file: %q %v", data, err)
	}
}

// GL-119: a pending seed whose list is beside its journal rolls back and is
// abandoned like one whose list is inline.
func TestSidecarSeedRollsBackAndAbandons(t *testing.T) {
	for _, kind := range []string{"rollback", "abandon"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r, w, linked, id := sidecarSeed(t, "seed-file-1")
			if kind == "rollback" {
				if _, err := RecoverSeed(ctx, r, linked, id, true); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"a", "b", "c"} {
					if _, err := os.Lstat(filepath.Join(linked, "notes", name)); !os.IsNotExist(err) {
						t.Fatalf("rollback left notes/%s: %v", name, err)
					}
				}
				if len(load(t, w).Include) != 0 {
					t.Fatal("rollback left the selection")
				}
			} else {
				result, err := Abandon(ctx, r, linked, id)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"a", "b"} {
					if data, err := os.ReadFile(filepath.Join(linked, "notes", name)); err != nil || string(data) != "seeded" {
						t.Fatalf("abandon changed notes/%s: %q %v (%+v)", name, data, err, result)
					}
				}
			}
			if pending, err := Pending(ctx, r, linked); err != nil || len(pending) != 0 {
				t.Fatalf("pending after %s: %v %v", kind, pending, err)
			}
			if err := ReadDiagnostic(ctx, r, linked, func(d DiagnosticState) error {
				if len(d.Journals) != 1 || d.Journals[0].Err != nil || !d.Journals[0].Completed || len(d.OrphanSeedLists) != 0 {
					t.Fatalf("diagnosis after %s: %+v %v", kind, d.Journals, d.OrphanSeedLists)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// GL-119: a seed stopped between writing its file list and its journal
// changed nothing; doctor names the orphan list, and seeding again works.
func TestSeedStoppedBeforeItsJournalLeavesReportedOrphanList(t *testing.T) {
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
	blob, err := r.History(ctx, w.Store, strings.NewReader("seeded"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	planner, paths := largeSeedPlanner(main, strings.TrimSpace(string(blob)), 650)
	result, err := seed(ctx, r, linked, planner, func(name string) error {
		if name == "seed-files" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "injected interruption") {
		t.Fatalf("seed did not stop after writing its file list: %+v %v", result, err)
	}
	orphan := seedListPath(w, result.TransactionID)
	if pending, err := Pending(ctx, r, linked); err != nil || len(pending) != 0 {
		t.Fatalf("pending after a seed stopped before its journal: %v %v", pending, err)
	}
	if err := ReadDiagnostic(ctx, r, linked, func(d DiagnosticState) error {
		if d.OrphanErr != nil || len(d.OrphanSeedLists) != 1 || d.OrphanSeedLists[0] != orphan {
			t.Fatalf("orphan seed lists: %q %v", d.OrphanSeedLists, d.OrphanErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(linked, "notes")); !os.IsNotExist(err) {
		t.Fatalf("seed stopped before its journal changed the worktree: %v", err)
	}
	if _, err := Seed(ctx, r, linked, planner); err != nil {
		t.Fatalf("seed after an orphan list: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(linked, filepath.FromSlash(paths[0]))); err != nil || string(data) != "seeded" {
		t.Fatalf("%s: %q %v", paths[0], data, err)
	}
}

// GL-119: the guards name the kind of a pending transaction rather than
// calling every journal a configuration transaction.
func TestGuardSnapshotNamesPendingTransactionKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, r, w := fixture(t)
	id := "00000000-0000-4000-8000-000000000001"
	if err := os.MkdirAll(filepath.Join(w.Common, "gitlocal", "transactions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Common, "gitlocal", "transactions", "restore-"+id+".json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGuardSnapshot(ctx, r, repo.Root)
	if err == nil || !strings.Contains(err.Error(), "unfinished restore transaction "+id) || !strings.Contains(err.Error(), "recover --list") {
		t.Fatalf("guard snapshot with a pending restore: %v", err)
	}
}

// GL-119: every reader of seed journals (recovery, abandon, doctor, backup
// adoption, encryption migration) decodes through decodeSeed, which reports a
// missing or mismatched file list as a *SeedListError naming the list.
func TestDecodeSeedReportsLostListToEveryReader(t *testing.T) {
	const id = "0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69"
	journal := []byte(`{"schemaVersion": 1, "kind": "seed", "id": "` + id + `", "files": [], "filesDigest": "` + strings.Repeat("a", 64) + `"}`)
	for name, files := range map[string]func(string) ([]byte, error){
		"missing":  func(string) ([]byte, error) { return nil, os.ErrNotExist },
		"mismatch": func(string) ([]byte, error) { return []byte("{\"files\": []}\n"), nil },
	} {
		var listErr *SeedListError
		_, err := decodeSeed(journal, files)
		if !errors.As(err, &listErr) || listErr.ID != id || !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "seed-files/"+id+".json") {
			t.Fatalf("%s list: %v", name, err)
		}
		if err := VerifyCompletedSeedForBackup(journal, files, "", "seed-"+id+".json", secureblob.Policy{}, nil); !errors.As(err, &listErr) {
			t.Fatalf("%s list in backup adoption: %v", name, err)
		}
		if _, _, err := RewriteCompletedSeedJournal(journal, files, "", "", nil, nil); !errors.As(err, &listErr) {
			t.Fatalf("%s list in encryption migration: %v", name, err)
		}
	}
}
