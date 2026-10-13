package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testDir(t *testing.T) (*Dir, string) {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := OpenDir(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}
func TestAtomicWriteAndSafety(t *testing.T) {
	d, path := testDir(t)
	if err := d.MkdirAll("a/b"); err != nil {
		t.Fatal(err)
	}
	if err := d.AtomicWrite("a/b/state", []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := d.Fingerprint("a/b/state")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.AtomicWrite("a/b/state", []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	now, err := d.Fingerprint("a/b/state")
	if err != nil || old.Digest == now.Digest {
		t.Fatal("fingerprint did not change", err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(path, "a/b/state"))
		if fi.Mode().Perm() != 0600 {
			t.Fatal("broad metadata permissions")
		}
	}
	for _, p := range []string{"../escape", "a/../../escape", "/absolute", "a/../escape"} {
		if err = d.AtomicWrite(p, []byte("bad"), 0600); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	if err = os.Link(filepath.Join(path, "a/b/state"), filepath.Join(path, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ReadFile("alias", 100); err == nil {
		t.Fatal("accepted hard-linked read")
	}
	if err = d.AtomicWrite("alias", []byte("bad"), 0600); err == nil {
		t.Fatal("accepted hard-linked replacement")
	}
}
func TestNoFollowReadsAndReplacements(t *testing.T) {
	d, path := testDir(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600)
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Skip(err)
	}
	if _, err := d.ReadFile("link/keep", 100); err == nil {
		t.Fatal("followed directory link")
	}
	if err := d.AtomicWrite("link/keep", []byte("bad"), 0600); err == nil {
		t.Fatal("wrote through directory link")
	}
	if err := os.Symlink(filepath.Join(outside, "keep"), filepath.Join(path, "file")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadFile("file", 100); err == nil {
		t.Fatal("followed file link")
	}
	if err := d.AtomicWrite("file", []byte("bad"), 0600); err == nil {
		t.Fatal("replaced symlink")
	}
	b, _ := os.ReadFile(filepath.Join(outside, "keep"))
	if string(b) != "keep" {
		t.Fatal("outside bytes changed")
	}
}
func TestLockCancellationAndSharing(t *testing.T) {
	d, _ := testDir(t)
	ctx := context.Background()
	a, err := d.Lock(ctx, "lock", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Lock(ctx, "lock", true)
	if err != nil {
		t.Fatal(err)
	}
	timeout, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if _, err = d.Lock(timeout, "lock", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("exclusive lock did not wait", err)
	}
	a.Close()
	b.Close()
	c, err := d.Lock(ctx, "lock", false)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
func TestLockReleasedWhenProcessDies(t *testing.T) {
	if dir := os.Getenv("GITLOCAL_TEST_LOCK_DIR"); dir != "" {
		d, err := OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		lock, err := d.Lock(context.Background(), "lock", false)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err = os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Second)
		return
	}
	d, path := testDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockReleasedWhenProcessDies$")
	cmd.Env = append(os.Environ(), "GITLOCAL_TEST_LOCK_DIR="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(path, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	timeout, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := d.Lock(timeout, "lock", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	timeout2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	lock, err := d.Lock(timeout2, "lock", false)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}
func TestMetadataValidation(t *testing.T) {
	d, _ := testDir(t)
	d.AtomicWrite("state", []byte(`{"value":1} {}`), 0600)
	var v struct {
		Value int `json:"value"`
	}
	if err := d.ReadJSON("state", &v); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	d.AtomicWrite("state", []byte(`{"unknown":1}`), 0600)
	if err := d.ReadJSON("state", &v); err == nil {
		t.Fatal("accepted unknown field")
	}
	for _, p := range []string{".git/config", "a/.GIT/config", "../secret", "a\nfile", "CON", "a./file"} {
		if err := ValidateSelectionPath(p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	if AliasKey("CAFÉ") != AliasKey("cafe\u0301") {
		t.Fatal("Unicode alias missed")
	}
	id, err := UUID()
	if err != nil || !ValidUUID(id) || ValidUUID(strings.ToUpper(id)) {
		t.Fatal("UUID validation")
	}
}

func TestOpeningSymlinkedAncestorRejected(t *testing.T) {
	_, path := testDir(t)
	target := t.TempDir()
	os.Mkdir(filepath.Join(target, "child"), 0700)
	if err := os.Symlink(target, filepath.Join(path, "alias")); err != nil {
		t.Skip(err)
	}
	if d, err := OpenDir(filepath.Join(path, "alias", "child")); err == nil {
		d.Close()
		t.Fatal("followed symlinked ancestor")
	}
}

// GL-119: WriteJSON refuses metadata ReadJSON couldn't read back, before
// writing anything.
func TestWriteJSONRefusesWhatReadJSONCantRead(t *testing.T) {
	d, err := OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	large := map[string]string{"data": strings.Repeat("a", 1<<20)}
	if err := d.WriteJSON("large.json", large); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized metadata written: %v", err)
	}
	if _, err := d.Lstat("large.json"); !os.IsNotExist(err) {
		t.Fatalf("refused metadata left a file: %v", err)
	}
	small := map[string]string{"data": strings.Repeat("a", 1<<19)}
	if err := d.WriteJSON("small.json", small); err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := d.ReadJSON("small.json", &back); err != nil || back["data"] != small["data"] {
		t.Fatalf("metadata under the limit: %v", err)
	}
}
