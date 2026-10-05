package configtx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func completedOutcome(t *testing.T, w *workspace.Context, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.Common, "gitlocal", "transactions", "completed", name))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record.Outcome
}

func TestAbandonConfigurationTransactionThatCannotRecover(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, "local")
	ctx := context.Background()
	result, err := change(ctx, g, r.Root, true, func(s string) error {
		if s == "union" {
			return errors.New("stop")
		}
		return nil
	}, "local")
	if err == nil {
		t.Fatal("fault missed")
	}
	before, _ := os.ReadFile(w.Exclude)
	edited := append(before, []byte("# external edit\n")...)
	if err := os.WriteFile(w.Exclude, edited, 0600); err != nil {
		t.Fatal(err)
	}
	for _, rollback := range []bool{false, true} {
		if _, err = Recover(ctx, g, r.Root, result.TransactionID, rollback); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("recovery (rollback=%v) didn't stop: %v", rollback, err)
		}
	}
	if _, err := LoadGuardSnapshot(ctx, g, r.Root); err == nil {
		t.Fatal("guard snapshot ignored the pending journal")
	}
	pending, err := PendingJournals(ctx, g, r.Root)
	if err != nil || len(pending) != 1 || pending[0].ID != result.TransactionID || pending[0].Kind != "config" {
		t.Fatalf("pending journals: %+v %v", pending, err)
	}
	if action, _ := pending[0].Next(); !strings.Contains(action, "--abandon") {
		t.Fatalf("next action lacks abandon: %s", action)
	}
	abandoned, err := Abandon(ctx, g, r.Root, result.TransactionID)
	if err != nil || abandoned.Kind != "config" || abandoned.Quarantine != "" {
		t.Fatalf("abandon: %+v %v", abandoned, err)
	}
	if after, _ := os.ReadFile(w.Exclude); !bytes.Equal(after, edited) {
		t.Fatal("abandon changed info/exclude")
	}
	if got := completedOutcome(t, w, "config-"+result.TransactionID+".json"); got != OutcomeAbandoned {
		t.Fatalf("completed outcome %q", got)
	}
	if _, err := LoadGuardSnapshot(ctx, g, r.Root); err != nil {
		t.Fatalf("guards still blocked: %v", err)
	}
	if ids, err := Pending(ctx, g, r.Root); err != nil || len(ids) != 0 {
		t.Fatalf("pending after abandon: %v %v", ids, err)
	}
	if err := ReadDiagnostic(ctx, g, r.Root, func(d DiagnosticState) error {
		if d.JournalErr != nil || len(d.Journals) != 1 || d.Journals[0].Err != nil || !d.Journals[0].Completed {
			t.Fatalf("abandoned journal diagnosis: %+v %v", d.Journals, d.JournalErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Abandon(ctx, g, r.Root, result.TransactionID); err != nil {
		t.Fatalf("abandon isn't idempotent: %v", err)
	}
	if _, err := Recover(ctx, g, r.Root, result.TransactionID, false); err == nil {
		t.Fatal("resume accepted an abandoned transaction")
	}
}

func TestAbandonSeedQuarantinesUnrecognizedStageAndKeepsUserFiles(t *testing.T) {
	repo, r, main := fixture(t)
	repo.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed", linked)
	w, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	blob := func(content string) string {
		out, err := r.History(ctx, w.Store, strings.NewReader(content), "hash-object", "-w", "--stdin", "--no-filters")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	a, b := blob("seeded-a"), blob("seeded-b")
	planner := func(_ workspace.Context) (SeedPlan, error) {
		state := selection.Empty()
		state.Generation = 1
		state.Include = []selection.Rule{{Path: "notes", Kind: "directory"}}
		return SeedPlan{SourceWorkspace: main.WorkspaceID, SourceSnapshot: a, Selection: state, Files: []SeedFile{{Path: "notes/a", Mode: "100644", Blob: a}, {Path: "notes/b", Mode: "100644", Blob: b}}}, nil
	}
	result, err := seed(ctx, r, linked, planner, func(name string) error {
		if name == "seed-journal" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if err == nil {
		t.Fatal("seed didn't stop")
	}
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	var j seedJournal
	if err := meta.ReadJSON(seedPath(result.TransactionID), &j); err != nil {
		t.Fatal(err)
	}
	// A user writes notes/a after the interruption, and a partial stage sits
	// beside it: neither direction can finish without discarding user bytes.
	if err := os.MkdirAll(filepath.Join(linked, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "notes", "a"), []byte("user edit"), 0644); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(linked, filepath.FromSlash(j.Files[0].Stage))
	if err := os.WriteFile(partial, []byte("seed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverSeed(ctx, r, linked, result.TransactionID, false); err == nil {
		t.Fatal("resume overwrote a user edit")
	}
	if _, err := RecoverSeed(ctx, r, linked, result.TransactionID, true); err == nil {
		t.Fatal("rollback removed a user edit")
	}
	// A complete stage for notes/b duplicates local history and may go.
	complete := filepath.Join(linked, filepath.FromSlash(j.Files[1].Stage))
	if err := os.WriteFile(complete, []byte("seeded-b"), 0644); err != nil {
		t.Fatal(err)
	}
	abandoned, err := Abandon(ctx, r, linked, result.TransactionID)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if len(abandoned.Removed) != 1 || abandoned.Removed[0] != j.Files[1].Stage || len(abandoned.Quarantined) != 1 || abandoned.Quarantined[0].Source != j.Files[0].Stage {
		t.Fatalf("abandon result: %+v", abandoned)
	}
	kept, err := os.ReadFile(filepath.Join(abandoned.Quarantine, filepath.FromSlash(abandoned.Quarantined[0].File)))
	if err != nil || string(kept) != "seed" {
		t.Fatalf("quarantined bytes: %q %v", kept, err)
	}
	if !strings.HasPrefix(abandoned.Quarantine, filepath.Join(w.Common, "gitlocal", "quarantine")) {
		t.Fatalf("quarantine location %s", abandoned.Quarantine)
	}
	for _, stage := range []string{partial, complete} {
		if _, err := os.Lstat(stage); !os.IsNotExist(err) {
			t.Fatalf("stage %s remains: %v", stage, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(linked, "notes", "a")); err != nil || string(data) != "user edit" {
		t.Fatalf("user file changed: %q %v", data, err)
	}
	if got := completedOutcome(t, w, "seed-"+result.TransactionID+".json"); got != OutcomeAbandoned {
		t.Fatalf("completed outcome %q", got)
	}
	if _, err := LoadGuardSnapshot(ctx, r, linked); err != nil {
		t.Fatalf("guards still blocked: %v", err)
	}
	if _, err := workspace.Init(ctx, r, linked); err != nil {
		t.Fatalf("init after abandon: %v", err)
	}
	var manifest Quarantine
	if err := meta.ReadJSON(filepath.Join("quarantine", result.TransactionID, "manifest.json"), &manifest); err != nil || len(manifest.Entries) != 1 || manifest.Kind != "seed" {
		t.Fatalf("quarantine manifest: %+v %v", manifest, err)
	}
}

func TestPendingInitJournalOfRemovedCheckoutPointsToAbandon(t *testing.T) {
	_, g, w := fixture(t)
	ctx := context.Background()
	id, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(w.Common, "gitlocal", "transactions"), 0700); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	journal := `{"schemaVersion":1,"kind":"init","identity":{"workspaceId":"` + id + `"},"workspace":{"adminPath":` + strconvQuote(filepath.Join(gone, "admin")) + `,"rootPath":` + strconvQuote(gone) + `}}`
	if err := os.WriteFile(filepath.Join(w.Common, "gitlocal", "transactions", "init.json"), []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingJournals(ctx, g, w.Root)
	if err != nil || len(pending) != 1 || pending[0].Kind != "init" || pending[0].ID != id || !pending[0].AdminMissing {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	if action, _ := pending[0].Next(); action != "Run git local recover "+id+" --abandon." {
		t.Fatalf("action: %s", action)
	}
	if kind, err := JournalKind(ctx, g, w.Root, id); err != nil || kind != "init" {
		t.Fatalf("kind: %q %v", kind, err)
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
