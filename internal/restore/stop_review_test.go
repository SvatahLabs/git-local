package restore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
)

// stoppedRestore leaves a restore of notes/a stopped at an external edit it
// preserved, and returns its ID.
func stoppedRestore(t *testing.T) (string, *gitexec.Runner, string) {
	t.Helper()
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	write(t, root, "notes/b", "kept")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	remove(t, root, "notes/a")
	save(t, r, root)
	result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
		if at == "before-install:0" {
			write(t, root, "notes/a", "external")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "move notes/a aside") {
		t.Fatalf("restore over an external edit: %v", err)
	}
	return root, r, result.TransactionID
}

// GL-124 review: a restore stopped at an edit after its rollback started
// names only --rollback on every surface, since resume refuses. Before, the
// stop error, recover --list and the pending refusal all said "--resume or
// --rollback".
func TestRestoreStoppedDuringRollbackNamesOnlyRollback(t *testing.T) {
	t.Parallel()
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	write(t, root, "notes/b", "kept")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	remove(t, root, "notes/a")
	save(t, r, root)
	crash := errors.New("crash")
	result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
		if at == "before-install:0" {
			return crash
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "(crash)") {
		t.Fatalf("restore: %v", err)
	}
	id := result.TransactionID
	if _, err := recoverRestore(ctx, r, root, id, true, func(at string) error {
		if at == "rollback-intent" {
			return crash
		}
		return nil
	}); err == nil || !strings.Contains(err.Error(), "(crash)") {
		t.Fatalf("rollback: %v", err)
	}
	write(t, root, "notes/a", "external")
	onlyRollback := func(what, message string) {
		t.Helper()
		if strings.Contains(message, "--resume") || !strings.Contains(message, "recover "+id+" --rollback") || !strings.Contains(message, "recover "+id+" --abandon") || !strings.Contains(strings.ToLower(message), "move notes/a aside") {
			t.Fatalf("%s: %s", what, message)
		}
	}
	_, err = Recover(ctx, r, root, id, true)
	if err == nil {
		t.Fatal("rollback over an external edit succeeded")
	}
	onlyRollback("the stop error", err.Error())
	pending, err := configtx.PendingJournals(ctx, r, root)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	action, _ := pending[0].Next()
	onlyRollback("recover --list", action)
	if _, err := configtx.Change(ctx, r, root, true, "other"); err == nil {
		t.Fatal("track accepted with a pending restore")
	} else {
		onlyRollback("the pending refusal", err.Error())
	}
	if _, err := Recover(ctx, r, root, id, false); err == nil || !strings.Contains(err.Error(), "rollback has started") {
		t.Fatalf("resume during rollback: %v", err)
	}
	// The advice works.
	if err := os.Rename(filepath.Join(root, "notes", "a"), filepath.Join(t.TempDir(), "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, root, id, true); err != nil {
		t.Fatalf("rollback after moving the edit aside: %v", err)
	}
}

// GL-124 review: abandoning a stopped restore clears the stop, so the
// completed journal has no field earlier releases' strict decoding refuses.
// Before, it kept "stopped".
func TestAbandonStoppedRestoreDropsTheStop(t *testing.T) {
	t.Parallel()
	root, r, id := stoppedRestore(t)
	if _, err := Abandon(ctx, r, root, id); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".git", "gitlocal", "transactions", "completed", "restore-"+id+".json"))
	if err != nil || strings.Contains(string(data), `"stopped"`) || !strings.Contains(string(data), `"abandoned"`) {
		t.Fatalf("completed journal: %v\n%s", err, data)
	}
}

// GL-124 review: a crash between abandon's journal write and its archive
// leaves a pending journal whose outcome is "abandoned"; resume and rollback
// both refuse it, so every surface names abandon, and abandon finishes it.
// Before, they said "--resume or --rollback".
func TestAbandonInterruptedBeforeArchiveAdvisesAbandon(t *testing.T) {
	t.Parallel()
	root, r, id := stoppedRestore(t)
	meta, err := openMeta(filepath.Join(root, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(meta, journalPath(id))
	if err != nil || j.Stopped == nil {
		t.Fatalf("journal: %+v %v", j.Stopped, err)
	}
	// The state abandon leaves when it stops after writing the journal
	// (an earlier release's abandon also kept the stop).
	j.Outcome = configtx.OutcomeAbandoned
	err = writeJournal(meta, j)
	meta.Close()
	if err != nil {
		t.Fatal(err)
	}
	advise := func(what, message string) {
		t.Helper()
		if strings.Contains(message, "--resume") || strings.Contains(message, "--rollback") || !strings.Contains(message, "recover "+id+" --abandon") || !strings.Contains(message, "abandon has started") {
			t.Fatalf("%s: %s", what, message)
		}
	}
	pending, err := configtx.PendingJournals(ctx, r, root)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	action, state := pending[0].Next()
	advise("recover --list", action+" "+state)
	if _, err := configtx.Change(ctx, r, root, true, "other"); err == nil {
		t.Fatal("track accepted with a pending restore")
	} else {
		advise("the pending refusal", err.Error())
	}
	for _, rollback := range []bool{false, true} {
		if _, err := Recover(ctx, r, root, id, rollback); err == nil {
			t.Fatalf("recover (rollback %v) of an abandoned journal succeeded", rollback)
		}
	}
	if _, err := Abandon(ctx, r, root, id); err != nil {
		t.Fatalf("abandon again: %v", err)
	}
	if pending, err := configtx.PendingJournals(ctx, r, root); err != nil || len(pending) != 0 {
		t.Fatalf("pending after abandon: %+v %v", pending, err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".git", "gitlocal", "transactions", "completed", "restore-"+id+".json"))
	if err != nil || strings.Contains(string(data), `"stopped"`) {
		t.Fatalf("completed journal: %v\n%s", err, data)
	}
	if b, err := os.ReadFile(filepath.Join(root, "notes", "a")); err != nil || string(b) != "external" {
		t.Fatalf("notes/a: %q %v", b, err)
	}
}
