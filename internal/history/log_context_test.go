package history

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestLogFiltersHistoricalMainlineContextAcrossBranchMoves(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	initialHead := strings.TrimSpace(string(r.Git("rev-parse", "HEAD")))
	write(t, r.Root, "local", []byte("main first"))
	change(t, g, r.Root, true, "local")
	first := save(t, g, r.Root)
	r.Git("checkout", "-qb", "feature")
	write(t, r.Root, "local", []byte("feature first"))
	second := save(t, g, r.Root)
	write(t, r.Root, "app.txt", []byte("application feature\n"))
	r.Git("add", "app.txt")
	r.Git("commit", "-qm", "feature mainline commit")
	featureHead := strings.TrimSpace(string(r.Git("rev-parse", "HEAD")))
	write(t, r.Root, "local", []byte("feature second"))
	third := save(t, g, r.Root)
	r.Git("checkout", "-q", "main")
	write(t, r.Root, "local", []byte("main second"))
	fourth := save(t, g, r.Root)
	r.Git("branch", "-m", "feature", "renamed")

	before := durableState(t, w.Store)
	check := func(filter LogFilter, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		entries, err := LogFiltered(context.Background(), g, r.Root, "", 20, filter)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(entries))
		for _, entry := range entries {
			got = append(got, entry.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("filter %+v: got %v, want %v", filter, got, want)
		}
	}
	check(LogFilter{Branch: "main"}, fourth.ID, first.ID)
	check(LogFilter{Branch: "refs/heads/feature"}, third.ID, second.ID)
	check(LogFilter{Branch: "renamed"})
	check(LogFilter{MainCommit: initialHead}, fourth.ID, second.ID, first.ID)
	check(LogFilter{MainCommit: strings.ToUpper(featureHead)}, third.ID)
	check(LogFilter{Branch: "feature", MainCommit: initialHead}, second.ID)
	check(LogFilter{Branch: "feature", MainCommit: featureHead}, third.ID)
	check(LogFilter{Branch: "feature"}, third.ID, second.ID)
	limited, err := LogFiltered(context.Background(), g, r.Root, "", 1, LogFilter{Branch: "feature"})
	if err != nil || len(limited) != 1 || limited[0].ID != third.ID {
		t.Fatalf("limit counted skipped snapshots: %+v, %v", limited, err)
	}
	if _, err := LogFiltered(context.Background(), g, r.Root, "", 20, LogFilter{MainCommit: initialHead[:8]}); err == nil {
		t.Fatal("abbreviated mainline commit ID accepted")
	}
	if _, err := LogFiltered(context.Background(), g, r.Root, "", 20, LogFilter{Branch: "bad..name"}); err == nil {
		t.Fatal("invalid branch name accepted")
	}
	if after := durableState(t, w.Store); !reflect.DeepEqual(after, before) {
		t.Fatal("filtered log changed private history")
	}
}
