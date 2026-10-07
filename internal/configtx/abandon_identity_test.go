package configtx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// TestUnreadableWorktreeIdentityIsNamed: abandoning an initialization journal
// checks every worktree identity file. A corrupt one must be named, in the
// abandon error and in doctor's finding, rather than blamed on the journal.
func TestUnreadableWorktreeIdentityIsNamed(t *testing.T) {
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
	repo := `{"schemaVersion":1,"repositoryId":"` + w.RepositoryID + `"}`
	journal := `{"schemaVersion":1,"kind":"init","repository":` + repo + `,"identity":{"schemaVersion":1,"repositoryId":"` + w.RepositoryID + `","workspaceId":"` + id + `"},"workspace":{"schemaVersion":1,"workspaceId":"` + id + `","createdAt":"2026-10-06T00:00:00Z","adminPath":` + strconvQuote(filepath.Join(gone, "admin")) + `,"rootPath":` + strconvQuote(gone) + `,"state":"active"},"newRepository":false,"selectionSchema":2}`
	if err := os.WriteFile(filepath.Join(w.Common, "gitlocal", "transactions", "init.json"), []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(w.Common, "worktrees", "other")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(other, "gitlocal-workspace.json")
	if err := os.WriteFile(identity, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingJournals(ctx, g, w.Root)
	if err != nil || len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	action, state := pending[0].Next()
	if !strings.Contains(state, identity) || strings.Contains(state, "journal can't be read") || !strings.Contains(action, identity) {
		t.Fatalf("doctor's finding: %q / %q", state, action)
	}
	if _, err := workspace.AbandonInit(ctx, g, w.Root, id); err == nil || !strings.Contains(err.Error(), identity) {
		t.Fatalf("abandon error doesn't name the identity file: %v", err)
	}
	// Once the file is repaired, abandon proceeds.
	if err := os.Remove(identity); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.AbandonInit(ctx, g, w.Root, id); err != nil {
		t.Fatalf("abandon after repair: %v", err)
	}
}
