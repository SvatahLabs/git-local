package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWithinFollowsTheFilesystemsNameRules: containment must agree with the
// filesystem. A spelling that differs only in case or Unicode normalization
// is inside a directory exactly when it names the same directory: always on a
// default macOS volume, never on a case-sensitive one or on Linux.
func TestWithinFollowsTheFilesystemsNameRules(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "Café")
	if err := os.MkdirAll(filepath.Join(root, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "Café2"), 0700); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"Café", "café", "CAFÉ", "Café", "café"} {
		info, err := os.Stat(filepath.Join(base, spelling))
		same := err == nil && os.SameFile(info, rootInfo)
		for _, p := range []string{filepath.Join(base, spelling), filepath.Join(base, spelling, "hooks"), filepath.Join(base, spelling, "missing", "hooks")} {
			if got := within(root, p); got != same {
				t.Errorf("within(%q, %q) = %t; the filesystem says %t", root, p, got, same)
			}
		}
	}
	for _, p := range []string{filepath.Join(base, "Café2"), filepath.Join(base, "Café2", "hooks"), base} {
		if within(root, p) {
			t.Errorf("within(%q, %q) = true", root, p)
		}
	}
	if !within(filepath.Join(base, "missing"), filepath.Join(base, "missing", "hooks")) || within(filepath.Join(base, "missing"), filepath.Join(base, "Missing2")) {
		t.Error("containment in a missing root")
	}
}
