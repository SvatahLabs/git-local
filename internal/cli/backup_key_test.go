package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// From a linked worktree, a backup key must still stay out of the main
// checkout and the common Git directory, as for encrypt keygen.
func TestBackupKeyOutsideEveryCheckoutFromLinkedWorktree(t *testing.T) {
	repo := layoutRepo(t)
	linked := linkedWorkspace(t, repo, t.TempDir(), "backup-key")
	for _, path := range []string{filepath.Join(repo.Root, "backup.key"), filepath.Join(repo.Root, ".git", "backup.key"), filepath.Join(linked, "backup.key")} {
		code, _, diagnostic := callCLI(t, linked, "backup", "keygen", path)
		if code == 0 || !strings.Contains(diagnostic, "outside") {
			t.Fatalf("backup keygen %s from a linked worktree: exit %d %s", path, code, diagnostic)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refused key was written: %s %v", path, err)
		}
	}
	// A key someone put in the main checkout by hand is refused by push.
	inMain := filepath.Join(repo.Root, "placed.key")
	if err := os.WriteFile(inMain, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, diagnostic := callCLI(t, linked, "backup", "push", "backup", "--key-file", inMain); code == 0 || !strings.Contains(diagnostic, "outside every checkout") {
		t.Fatalf("backup push with a key in the main checkout: exit %d %s", code, diagnostic)
	}
	outside := filepath.Join(t.TempDir(), "backup.key")
	successful(t, linked, "backup", "keygen", outside)
	if info, err := os.Stat(outside); err != nil || info.Size() != 32 {
		t.Fatalf("backup key outside the checkouts: %v", err)
	}
}

// A linked worktree deleted without git worktree prune stays registered. No
// key can lie below it, so backup and encrypt keygen still work.
func TestKeygenIgnoresDeletedWorktree(t *testing.T) {
	repo := layoutRepo(t)
	gone := filepath.Join(t.TempDir(), "gone")
	repo.Git("worktree", "add", "-q", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"backup", "encrypt"} {
		key := filepath.Join(t.TempDir(), command+".key")
		successful(t, repo.Root, command, "keygen", key)
		if info, err := os.Stat(key); err != nil || info.Size() != 32 {
			t.Fatalf("%s keygen with a deleted worktree: %v", command, err)
		}
	}
}
