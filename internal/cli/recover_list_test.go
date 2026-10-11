package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// TestRecoverListShowsPendingEncryptionJournal: a pending encryption journal
// blocks every session, but recover --list must still list it with its kind
// and next command.
func TestRecoverListShowsPendingEncryptionJournal(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	root := repo.Root
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOKEN=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, root, "init")
	successful(t, root, "track", "secret.txt")
	successful(t, root, "commit", "-m", "plaintext")
	// An encryption migration interrupted after writing its journal. The
	// listing reads only the journal's name, kind and the policy's presence.
	id := "6f1c2a4e-8b7d-4c3e-9a10-2b3c4d5e6f70"
	transactions := filepath.Join(root, ".git", "gitlocal", "transactions")
	journal := `{"schemaVersion":1,"kind":"encrypt","id":"` + id + `"}`
	if err := os.WriteFile(filepath.Join(transactions, "encrypt-"+id+".json"), []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, diagnostic := callCLI(t, root, "status"); code != 3 || !strings.Contains(diagnostic, "unfinished encryption migration") {
		t.Fatalf("the journal doesn't block sessions: exit %d %s", code, diagnostic)
	}
	code, out, diagnostic := callCLI(t, root, "recover", "--list")
	if code != 0 || !strings.Contains(out, id+" encrypt") || !strings.Contains(out, "git local encrypt recover "+id+" --resume") {
		t.Fatalf("recover --list: exit %d\n%s%s", code, out, diagnostic)
	}
	code, out, diagnostic = callCLI(t, root, "recover", "--list", "--json")
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			TransactionIDs []string `json:"transactionIds"`
			Transactions   []struct {
				ID, Kind, Next, State string
			} `json:"transactions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || code != 0 || !envelope.OK {
		t.Fatalf("recover --list --json: exit %d %v\n%s%s", code, err, out, diagnostic)
	}
	if len(envelope.Data.TransactionIDs) != 1 || envelope.Data.TransactionIDs[0] != id || len(envelope.Data.Transactions) != 1 {
		t.Fatalf("listed %+v", envelope.Data)
	}
	if listed := envelope.Data.Transactions[0]; listed.ID != id || listed.Kind != "encrypt" || !strings.Contains(listed.Next, "git local encrypt recover "+id+" --resume") || listed.State == "" {
		t.Fatalf("listed %+v", listed)
	}
}

// TestAutomaticEventPrintsSnapshotWarnings checks that a hook-driven snapshot
// shows the warning a manual commit shows, here for an unavailable workspace.
func TestAutomaticEventPrintsSnapshotWarnings(t *testing.T) {
	repo, write := unavailableWorkspaceFixture(t)
	write(repo.Root, "notes/plain.txt", "plain v2\n")
	code, _, diagnostic := callCLI(t, repo.Root, "auto", "event", "post-commit")
	if code != 0 {
		t.Fatalf("auto event: exit %d: %s", code, diagnostic)
	}
	if !strings.Contains(diagnostic, "git-local: warning:") || !strings.Contains(diagnostic, "is unavailable") {
		t.Fatalf("automatic snapshot dropped its warning: %q", diagnostic)
	}
	if out := successful(t, repo.Root, "show", "latest:notes/plain.txt"); out != "plain v2\n" {
		t.Fatalf("automatic snapshot didn't save the other file: %q", out)
	}
}

// TestRecoverListKeepsTransactionIDsToUUIDs: an unknown file and a corrupt
// initialization journal are listed in transactions, but transactionIds holds
// only IDs recover accepts.
func TestRecoverListKeepsTransactionIDsToUUIDs(t *testing.T) {
	repo, _ := unavailableWorkspaceFixture(t)
	transactions := filepath.Join(repo.Root, ".git", "gitlocal", "transactions")
	if err := os.MkdirAll(transactions, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"notes.txt": "left by hand\n", "init.json": "{corrupt"} {
		if err := os.WriteFile(filepath.Join(transactions, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, out, diagnostic := callCLI(t, repo.Root, "recover", "--list", "--json")
	var envelope struct {
		Data struct {
			TransactionIDs []string `json:"transactionIds"`
			Transactions   []struct {
				ID, Kind string
			} `json:"transactions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || len(envelope.Data.Transactions) != 2 {
		t.Fatalf("recover --list --json: %v\n%s%s", err, out, diagnostic)
	}
	if envelope.Data.TransactionIDs == nil || len(envelope.Data.TransactionIDs) != 0 {
		t.Fatalf("transactionIds lists non-UUID entries: %q", envelope.Data.TransactionIDs)
	}
	kinds := map[string]bool{}
	for _, listed := range envelope.Data.Transactions {
		kinds[listed.Kind] = true
	}
	if !kinds["unknown"] || !kinds["init"] {
		t.Fatalf("transactions: %+v", envelope.Data.Transactions)
	}
}
