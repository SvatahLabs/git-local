package configtx

import (
	"strings"
	"testing"
)

// GL-124 review: once a restore's rollback has started, resume refuses
// ("rollback has started; use --rollback to finish it"), so a stop during
// the rollback names only --rollback. Before, it said "--resume or
// --rollback".
func TestRestoreStopDuringRollbackAdvisesOnlyRollback(t *testing.T) {
	t.Parallel()
	id := "00000000-0000-4000-8000-000000000001"
	for _, stop := range []RestoreStop{{Kind: "edit", Path: "notes/a", Aside: true}, {Kind: "edit", Path: "notes/a"}, {Kind: "stage", Path: "notes/.gitlocal-tmp-x"}} {
		action, state := PendingJournal{ID: id, Kind: "restore", Root: "/w", Direction: "rollback", Stopped: &stop}.Next()
		if strings.Contains(action, "--resume") || !strings.Contains(action, "recover "+id+" --rollback from /w") || !strings.Contains(action, "recover "+id+" --abandon from /w") {
			t.Fatalf("%+v during rollback: %s", stop, action)
		}
		if !strings.Contains(state, "during its rollback") {
			t.Fatalf("%+v state: %s", stop, state)
		}
		if action, _ := (PendingJournal{ID: id, Kind: "restore", Direction: "apply", Stopped: &stop}).Next(); !strings.Contains(action, "--resume or --rollback") {
			t.Fatalf("%+v before rollback: %s", stop, action)
		}
	}
}

// GL-124 review: a journal abandon wrote but didn't archive is finished by
// running abandon again; resume and rollback both refuse it.
func TestAbandonedPendingJournalAdvisesAbandon(t *testing.T) {
	t.Parallel()
	id := "00000000-0000-4000-8000-000000000001"
	for _, kind := range []string{"restore", "seed", "config"} {
		action, state := PendingJournal{ID: id, Kind: kind, Direction: "apply", Outcome: OutcomeAbandoned, Stopped: &RestoreStop{Kind: "edit", Path: "a"}}.Next()
		if !strings.HasPrefix(action, "Run git local recover "+id+" --abandon") || strings.Contains(action, "--resume") || strings.Contains(action, "--rollback") {
			t.Fatalf("%s: %s", kind, action)
		}
		if !strings.Contains(state, "abandon has started") {
			t.Fatalf("%s state: %s", kind, state)
		}
	}
}
