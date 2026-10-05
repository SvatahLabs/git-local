package overlay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedAttributesPreserveUserBytesAndMatchLiteralNames(t *testing.T) {
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	user := []byte("*.txt custom=user\n# trailing user comment\n")
	paths := []string{"config/space name", "config/star*name", "config/hash#name", "config/ümlaut"}
	data, err := RenderAttributes(user, paths)
	if err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(root, ".git", "info")
	if err := os.MkdirAll(info, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "attributes"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		cmd := exec.Command(git, "check-attr", "filter", "--", path)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "gitlocal-overlay") {
			t.Fatalf("literal pattern %q: %v %s", path, err, out)
		}
	}
	cmd := exec.Command(git, "check-attr", "filter", "--", "config/starXname")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(out), "gitlocal-overlay") {
		t.Fatalf("wildcard escaped incorrectly: %v %s", err, out)
	}
	recovered, err := AttributeUserBytes(data, paths)
	if err != nil || !bytes.Equal(recovered, user) {
		t.Fatalf("user attributes changed: %v %q", err, recovered)
	}
	data[0] = 'X'
	if _, err := AttributeUserBytes(data, paths); err == nil {
		t.Fatal("edited managed block accepted")
	}
}
