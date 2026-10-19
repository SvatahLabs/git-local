package testutil

import (
	"path/filepath"
	"testing"
)

// The reftable probe ignores an inherited GIT_DIR, such as a hook's, which
// made git init refuse to re-initialize that repository with another ref
// backend and the reftable pass be skipped silently (GL-132).
func TestReftableProbeIgnoresInheritedGitEnvironment(t *testing.T) {
	if !gitHasReftable() {
		t.Skip("this Git has no reftable backend")
	}
	repo := New(t)
	t.Setenv("GIT_DIR", filepath.Join(repo.Root, ".git"))
	t.Setenv("GIT_WORK_TREE", repo.Root)
	if ok, diagnostic := ReftableSupported(); !ok {
		t.Fatalf("reftable probe with an inherited GIT_DIR: %s", diagnostic)
	}
}

// git clone doesn't take the source's ref format; RefFormatArgs gives a clone
// the format of the pass.
func TestFixtureRefFormatsOnReftable(t *testing.T) {
	if !gitHasReftable() {
		t.Skip("this Git has no reftable backend")
	}
	t.Cleanup(func() { RefFormat = "" })
	for _, format := range []string{"", "reftable"} {
		RefFormat = format
		CheckFixtureRefFormats(t)
	}
}
