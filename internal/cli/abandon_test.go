package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestRecoverAbandonCLIAndDoctorGuidance(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(root, "local.txt"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, root, "init")
	successful(t, root, "track", "local.txt")
	// Turn the completed track journal back into a pending one.
	transactions := filepath.Join(root, ".git", "gitlocal", "transactions")
	entries, err := os.ReadDir(filepath.Join(transactions, "completed"))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	name := entries[0].Name()
	id := strings.TrimSuffix(strings.TrimPrefix(name, "config-"), ".json")
	data, err := os.ReadFile(filepath.Join(transactions, "completed", name))
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]any
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	delete(journal, "outcome")
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transactions, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(transactions, "completed", name)); err != nil {
		t.Fatal(err)
	}
	code, out, _ := callCLI(t, root, "doctor")
	if code == 0 || !strings.Contains(out, "git local recover "+id+" --resume or --rollback") || !strings.Contains(out, "git local recover "+id+" --abandon") {
		t.Fatalf("doctor guidance (exit %d):\n%s", code, out)
	}
	if code, _, _ := callCLI(t, root, "recover", id, "--abandon", "--resume"); code != 2 {
		t.Fatalf("conflicting directions accepted: %d", code)
	}
	code, out, diagnostic := callCLI(t, root, "recover", id, "--abandon", "--json")
	if code != 0 {
		t.Fatalf("abandon exit %d: %s %s", code, out, diagnostic)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			TransactionID string `json:"transactionId"`
			Kind          string `json:"kind"`
			Abandoned     bool   `json:"abandoned"`
			Quarantine    string `json:"quarantine"`
			Quarantined   []any  `json:"quarantined"`
			Removed       []any  `json:"removed"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || !envelope.OK || envelope.Data.TransactionID != id || envelope.Data.Kind != "config" || !envelope.Data.Abandoned || envelope.Data.Quarantine != "" || envelope.Data.Quarantined == nil || envelope.Data.Removed == nil {
		t.Fatalf("abandon JSON: %s %v", out, err)
	}
	if out := successful(t, root, "recover", "--list"); !strings.Contains(out, "No unfinished transactions") {
		t.Fatal(out)
	}
	if out := successful(t, root, "recover", id, "--abandon"); !strings.Contains(out, "Abandoned config transaction "+id) {
		t.Fatal(out)
	}
	if out := successful(t, root, "doctor"); !strings.Contains(out, "Doctor found no blocking issues") {
		t.Fatal(out)
	}
	if code, _, diagnostic := callCLI(t, root, "recover", "00000000-0000-4000-8000-000000000000", "--abandon"); code == 0 || !strings.Contains(diagnostic, "no transaction") {
		t.Fatalf("unknown ID: %d %s", code, diagnostic)
	}
}

func TestRecoverDiscardRecoveryRequiresAbandon(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000000"
	for _, args := range [][]string{{"recover", id, "--resume", "--discard-recovery"}, {"recover", "--list", "--discard-recovery"}} {
		if code, _, diagnostic := callCLI(t, t.TempDir(), args...); code != 2 {
			t.Fatalf("%v: exit %d %s", args, code, diagnostic)
		}
	}
}
