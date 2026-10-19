package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

func TestEncryptionCommandsForFreshHistory(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// The documented relocation path works when the old key's directory is gone,
// for example on an ejected drive.
func TestRebindKeyAfterOldKeyDirectoryIsGone(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", ".env.local")
	drive := filepath.Join(t.TempDir(), "usb")
	if err := os.MkdirAll(filepath.Join(drive, "keys"), 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(drive, "keys", "sensitive.key")
	successful(t, repo.Root, "encrypt", "keygen", keyPath)
	successful(t, repo.Root, "encrypt", "enable", "--key-file", keyPath, "--", ".env.local")
	successful(t, repo.Root, "commit", "-m", "encrypted")
	home := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(home, "sensitive.key")
	if err := os.Rename(keyPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(drive); err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(home, "wrong.key")
	successful(t, repo.Root, "encrypt", "keygen", wrong)
	if code, _, diagnostic := callCLI(t, repo.Root, "encrypt", "rebind-key", wrong); code == 0 || !strings.Contains(diagnostic, "fingerprint") {
		t.Fatalf("rebind to an unrelated key: exit %d %s", code, diagnostic)
	}
	successful(t, repo.Root, "encrypt", "rebind-key", moved)
	if got := successful(t, repo.Root, "show", "latest:.env.local"); got != "TOKEN=private\n" {
		t.Fatalf("show after rebinding: %q", got)
	}
}

// GL-108: commands that need the key name the relocation command, not a raw
// path error, when the key's directory is gone.
func TestMissingKeyDirectoryNamesRebindKey(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", ".env.local")
	drive := filepath.Join(t.TempDir(), "usb")
	if err := os.MkdirAll(filepath.Join(drive, "keys"), 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(drive, "keys", "sensitive.key")
	successful(t, repo.Root, "encrypt", "keygen", keyPath)
	successful(t, repo.Root, "encrypt", "enable", "--key-file", keyPath, "--", ".env.local")
	successful(t, repo.Root, "commit", "-m", "encrypted")
	if err := os.Rename(drive, drive+"-ejected"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"commit", "-m", "needs the key"}, {"show", "latest:.env.local"}} {
		if code, _, diagnostic := callCLI(t, repo.Root, args...); code == 0 || !strings.Contains(diagnostic, "git local encrypt rebind-key") || strings.Contains(diagnostic, "lstat") {
			t.Fatalf("%v without the key's directory: exit %d %s", args, code, diagnostic)
		}
	}
	if err := os.Rename(drive+"-ejected", drive); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "commit", "-m", "key back")
	if got := successful(t, repo.Root, "show", "latest:.env.local"); got != "TOKEN=changed\n" {
		t.Fatalf("show with the key back: %q", got)
	}
}
