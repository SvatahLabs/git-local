package history

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestScopedSnapshotsPreserveParallelWriterEditsAndProvenance(t *testing.T) {
	repo, g, w := fixture(t)
	repo.Commit()
	write(t, repo.Root, "notes/a", []byte("a0"))
	write(t, repo.Root, "notes/b", []byte("b0"))
	change(t, g, repo.Root, true, "notes")
	baseline := save(t, g, repo.Root)
	write(t, repo.Root, "notes/a", []byte("a1"))
	write(t, repo.Root, "notes/b", []byte("b1"))
	first, err := CommitScoped(context.Background(), g, repo.Root, "A changes notes", "session-a", []string{"notes/a"})
	if err != nil || !first.Changed {
		t.Fatalf("scoped writer A snapshot: %+v %v", first, err)
	}
	if got := string(gitHistory(t, g, w, "cat-file", "blob", first.ID+":files/notes/a")); got != "a1" {
		t.Fatalf("writer A file: %q", got)
	}
	if got := string(gitHistory(t, g, w, "cat-file", "blob", first.ID+":files/notes/b")); got != "b0" {
		t.Fatalf("writer B's unsaved bytes entered writer A snapshot: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(repo.Root, "notes/b")); err != nil || string(got) != "b1" {
		t.Fatalf("scoped commit changed writer B's working file: %q %v", got, err)
	}
	_, metadata, err := readPrevious(context.Background(), g, w.Store, first.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil || metadata.Writer != "session-a" || !reflect.DeepEqual(metadata.Scope, []string{"notes/a"}) || !reflect.DeepEqual(metadata.ChangedPaths, []string{"notes/a"}) {
		t.Fatalf("writer A metadata: %+v %v", metadata, err)
	}
	write(t, repo.Root, "notes/a", []byte("a2"))
	second, err := commitWithOptions(context.Background(), g, repo.Root, "A again", "session-a", []string{"notes/a"}, func(at string) error {
		if at == "before-publish" {
			write(t, repo.Root, "notes/b", []byte("b2"))
			write(t, repo.Root, "notes/c", []byte("c1"))
		}
		return nil
	})
	if err != nil || !second.Changed {
		t.Fatalf("out-of-scope concurrent edit blocked snapshot: %+v %v", second, err)
	}
	if got := string(gitHistory(t, g, w, "cat-file", "blob", second.ID+":files/notes/b")); got != "b0" {
		t.Fatalf("concurrent writer B edit was captured: %q", got)
	}
	if _, err := g.History(context.Background(), w.Store, nil, "cat-file", "-e", second.ID+":files/notes/c"); err == nil {
		t.Fatal("concurrent new file was captured outside scope")
	}
	third, err := CommitScoped(context.Background(), g, repo.Root, "B changes notes", "session-b", []string{"notes/b"})
	if err != nil || !third.Changed {
		t.Fatalf("scoped writer B snapshot: %+v %v", third, err)
	}
	if got := string(gitHistory(t, g, w, "cat-file", "blob", third.ID+":files/notes/a")); got != "a2" {
		t.Fatalf("writer A's saved bytes were lost: %q", got)
	}
	if got := string(gitHistory(t, g, w, "cat-file", "blob", third.ID+":files/notes/b")); got != "b2" {
		t.Fatalf("writer B's bytes were not captured: %q", got)
	}
	if again, err := CommitScoped(context.Background(), g, repo.Root, "B unchanged", "session-b", []string{"notes/b"}); err != nil || again.Changed || again.ID != third.ID {
		t.Fatalf("scoped no-op made a snapshot: %+v %v", again, err)
	}
	entries, err := LogFiltered(context.Background(), g, repo.Root, "", 20, LogFilter{Writer: "session-a", Path: "notes/a", WithProvenance: true})
	if err != nil || len(entries) != 2 || entries[0].ID != second.ID || entries[1].ID != first.ID || !strings.Contains(FormatLog(entries), "writer: session-a") {
		t.Fatalf("writer/path query: %+v %v", entries, err)
	}
	if err := os.Remove(filepath.Join(repo.Root, "notes/a")); err != nil {
		t.Fatal(err)
	}
	deleted, err := CommitScoped(context.Background(), g, repo.Root, "A deletes file", "session-a", []string{"notes/a"})
	if err != nil || !deleted.Changed {
		t.Fatalf("scoped deletion: %+v %v", deleted, err)
	}
	_, metadata, err = readPrevious(context.Background(), g, w.Store, deleted.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil || !reflect.DeepEqual(metadata.ChangedPaths, []string{"notes/a"}) {
		t.Fatalf("deletion provenance: %+v %v", metadata, err)
	}
	if got := string(gitHistory(t, g, w, "cat-file", "blob", deleted.ID+":files/notes/b")); got != "b2" {
		t.Fatalf("scoped deletion removed another writer's file: %q", got)
	}
	if baseline.ID == deleted.ID {
		t.Fatal("scoped sequence did not advance history")
	}
}

func TestScopedCommitRejectsOutsideScopeAndChangedSelection(t *testing.T) {
	repo, g, w := fixture(t)
	write(t, repo.Root, "notes/a", []byte("private"))
	write(t, repo.Root, "notes/b", []byte("another writer"))
	write(t, repo.Root, "other", []byte("not selected"))
	change(t, g, repo.Root, true, "notes")
	initial, err := CommitScoped(context.Background(), g, repo.Root, "initial scoped", "session-a", []string{"notes/a"})
	if err != nil || !initial.Changed {
		t.Fatalf("first scoped snapshot: %+v %v", initial, err)
	}
	save(t, g, repo.Root)
	for _, path := range []string{"other", "../escape", "/absolute", "notes/../other"} {
		if _, err := CommitScoped(context.Background(), g, repo.Root, "invalid", "session-a", []string{path}); err == nil {
			t.Fatalf("invalid scope %q accepted", path)
		}
	}
	if _, err := CommitScoped(context.Background(), g, repo.Root, "invalid", "bad writer", []string{"notes/a"}); err == nil {
		t.Fatal("invalid writer ID accepted")
	}
	change(t, g, repo.Root, true, "other")
	additive, err := CommitScoped(context.Background(), g, repo.Root, "new selection, scoped content", "session-a", []string{"notes/a"})
	if err != nil || !additive.Changed {
		t.Fatalf("additive selection blocked scoped commit: %+v %v", additive, err)
	}
	if _, err := g.History(context.Background(), w.Store, nil, "cat-file", "-e", additive.ID+":files/other"); err == nil {
		t.Fatal("newly selected out-of-scope file was captured")
	}
	change(t, g, repo.Root, false, "notes/b")
	// GL-137: the removed selection's saved file is dropped, not carried.
	removed, err := CommitScoped(context.Background(), g, repo.Root, "selection removed", "session-a", []string{"notes/a"})
	if err != nil || !removed.Changed {
		t.Fatalf("scoped commit after a removed selection: %+v %v", removed, err)
	}
	if _, err := g.History(context.Background(), w.Store, nil, "cat-file", "-e", removed.ID+":files/notes/b"); err == nil {
		t.Fatal("scoped commit carried an unselected saved file")
	}
	if _, err := Commit(context.Background(), g, repo.Root, "full selection update"); err != nil {
		t.Fatal(err)
	}
	if _, err := head(context.Background(), g, w.Store, Ref(w.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
}
