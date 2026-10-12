package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/secureblob"
)

// While a transaction is pending, encrypt rebind-key refuses, so a recovery
// that needs a missing key says to reconnect it at its path and names the
// other way out instead of sending the user to rebind-key.
func TestRecoveryNamesTheKeyPathNotRebind(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "ejected", "files.key")
	policy := secureblob.Policy{SchemaVersion: secureblob.PolicySchemaVersion, KeyFile: missing, KeyFingerprint: strings.Repeat("a", 64), Sensitive: []secureblob.PathRule{{Path: "notes.txt", Kind: "file"}}}
	_, err := secureblob.LoadKey(policy, filepath.Join(dir, "checkout"), filepath.Join(dir, "checkout", ".git"))
	if !errors.Is(err, secureblob.ErrKeyUnavailable) || !strings.Contains(err.Error(), "git local encrypt rebind-key") {
		t.Fatalf("missing key: %v", err)
	}
	id := "00000000-0000-4000-8000-000000000001"
	wrapped := recoveryKeyError(err, id, "if it can't be reconnected, git local recover "+id+" --abandon gives the transaction up")
	message := wrapped.Error()
	if !errors.Is(wrapped, secureblob.ErrKeyUnavailable) || !strings.Contains(message, missing) || !strings.Contains(message, "can't run while it is pending") || !strings.Contains(message, "--abandon") || strings.Contains(message, "run git local encrypt rebind-key") {
		t.Fatalf("recovery message: %v", message)
	}
	other := errors.New("something else")
	if recoveryKeyError(other, id, "x") != other || recoveryKeyError(nil, id, "x") != nil {
		t.Fatal("recoveryKeyError changed an unrelated error")
	}
}

// A key path given on the command line that doesn't exist is reported as
// such; rebinding is no remedy for it.
func TestMissingSuppliedKeyDoesNotSuggestRebind(t *testing.T) {
	repo := layoutRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.txt"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.txt")
	typo := filepath.Join(t.TempDir(), "no-such-dir", "files.key")
	for _, args := range [][]string{{"encrypt", "enable", "--key-file", typo, "--", "notes.txt"}, {"encrypt", "migrate", "--key-file", typo, "--", "notes.txt"}} {
		code, _, diagnostic := callCLI(t, repo.Root, args...)
		if code == 0 || !strings.Contains(diagnostic, "doesn't exist") || strings.Contains(diagnostic, "rebind-key") {
			t.Fatalf("%v: exit %d %s", args, code, diagnostic)
		}
	}
}
