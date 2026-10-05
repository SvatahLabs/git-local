package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

func TestEncryptionCommandsForFreshHistory(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	successful(t, repo.Root, "init")
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", ".env.local")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	successful(t, repo.Root, "encrypt", "keygen", keyPath)
	code, out, diagnostic := callCLI(t, repo.Root, "encrypt", "enable", "--key-file", keyPath, "--json", "--", ".env.local")
	if code != 0 {
		t.Fatalf("enable fresh history: exit %d stdout %q stderr %q", code, out, diagnostic)
	}
	var response struct {
		OK   bool                       `json:"ok"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil || !response.OK || len(response.Data["sensitive"]) == 0 {
		t.Fatalf("encrypted enable JSON: %+v %v", response, err)
	}
	successful(t, repo.Root, "commit", "-m", "encrypted")
	if got := successful(t, repo.Root, "show", "latest:.env.local"); got != "TOKEN=private\n" {
		t.Fatalf("encrypted historical show: %q", got)
	}
	wrongKey := filepath.Join(t.TempDir(), "wrong.key")
	successful(t, repo.Root, "encrypt", "keygen", wrongKey)
	if code, _, _ := callCLI(t, repo.Root, "encrypt", "rebind-key", wrongKey); code == 0 {
		t.Fatal("rebind accepted unrelated key")
	}
	movedKey := filepath.Join(t.TempDir(), "moved.key")
	if err := os.Rename(keyPath, movedKey); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := callCLI(t, repo.Root, "show", "latest:.env.local"); code == 0 {
		t.Fatal("missing original key did not fail closed")
	}
	successful(t, repo.Root, "encrypt", "rebind-key", movedKey)
	if got := successful(t, repo.Root, "show", "latest:.env.local"); got != "TOKEN=private\n" {
		t.Fatalf("encrypted show after rebind: %q", got)
	}
}

func TestEncryptionMigrateExistingHistory(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	successful(t, repo.Root, "init")
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=old-private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", ".env.local")
	successful(t, repo.Root, "commit", "-m", "before encryption")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	successful(t, repo.Root, "encrypt", "keygen", keyPath)
	code, out, diagnostic := callCLI(t, repo.Root, "encrypt", "migrate", "--key-file", keyPath, "--json", "--", ".env.local")
	if code != 0 {
		t.Fatalf("migrate command: exit %d stdout %q stderr %q", code, out, diagnostic)
	}
	var response struct {
		OK   bool                       `json:"ok"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil || !response.OK || len(response.Data["transactionId"]) == 0 {
		t.Fatalf("migrate JSON: %+v %v", response, err)
	}
	if got := successful(t, repo.Root, "show", "latest:.env.local"); got != "TOKEN=old-private\n" {
		t.Fatalf("migrated historical show: %q", got)
	}
}
