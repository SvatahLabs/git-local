package overlay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	paths := []string{"config/space name", "config/hash#name", "config/ümlaut"}
	if runtime.GOOS != "windows" { // Windows forbids * in names.
		paths = append(paths, "config/star*name")
	}
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

func TestManagedAttributesUseGitQuotingForUnusualNames(t *testing.T) {
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	paths := []string{"nbsp\u00a0name", "ideographic\u3000space", "zero\u200bwidth", "tab\tname", "control\x01name", "del\x7fname"}
	if runtime.GOOS != "windows" { // Windows forbids " in names.
		paths = append(paths, "quote\"back")
	}
	data, err := RenderAttributes(nil, paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "attributes"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		cmd := exec.Command(git, "check-attr", "-z", "filter", "--", path)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != path+"\x00filter\x00gitlocal-overlay\x00" {
			t.Fatalf("pattern for %q does not match: %v %q\n%s", path, err, out, data)
		}
	}
}

func TestAttributeUserBytesAcceptsLegacyGoQuotedBlock(t *testing.T) {
	paths := []string{"nbsp\u00a0name", "plain"}
	legacy, err := renderBlock(paths, legacyAttrPattern)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(legacy, []byte(`\u00a0`)) {
		t.Fatalf("fixture is not the legacy quoting: %q", legacy)
	}
	user := []byte("*.txt custom=user\n")
	recovered, err := AttributeUserBytes(append(legacy, user...), paths)
	if err != nil || !bytes.Equal(recovered, user) {
		t.Fatalf("legacy block not recognized: %v %q", err, recovered)
	}
}
