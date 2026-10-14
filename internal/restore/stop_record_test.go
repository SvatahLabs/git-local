package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
)

// GL-124: a restore stopped at an edit it preserved records that in its
// journal, so recover --list, doctor and every other command's refusal name
// the way out (move the file aside, or abandon), not resume and rollback,
// which both refuse; once the edit is resolved, recovery clears the record.
// Before, they said "run git local recover <id> --resume or --rollback".
func TestRestoreStoppedAtEditNamesTheWayOutEverywhere(t *testing.T) {
	t.Parallel()
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
	id := result.TransactionID
	wayOut := func(what, message string) {
		t.Helper()
		if !strings.Contains(strings.ToLower(message), "move notes/a aside") || !strings.Contains(message, "recover "+id+" --abandon") {
			t.Fatalf("%s doesn't name the way out: %s", what, message)
		}
	}
	pending, err := configtx.PendingJournals(ctx, r, root)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	action, state := pending[0].Next()
	wayOut("recover --list", action)
	if !strings.Contains(state, "edit of notes/a") {
		t.Fatalf("recover --list state: %q", state)
	}
	if _, err := configtx.Change(ctx, r, root, true, "other"); err == nil {
		t.Fatal("track accepted with a pending restore")
	} else {
		wayOut("the pending refusal", err.Error())
	}
	// Following the advice clears the record and finishes the restore.
	if err := os.Rename(filepath.Join(root, "notes", "a"), filepath.Join(t.TempDir(), "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(ctx, r, root, id, false); err != nil {
		t.Fatalf("resume after moving the edit aside: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "notes", "a")); err != nil || string(b) != "source" {
		t.Fatalf("notes/a after resume: %q %v", b, err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".git", "gitlocal", "transactions", "completed", "restore-"+id+".json"))
	if err != nil || strings.Contains(string(data), `"stopped"`) {
		t.Fatalf("completed journal keeps the stop: %v\n%s", err, data)
	}
}
