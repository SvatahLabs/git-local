package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSmokeExistingRejectsTamperedNativeArtifact(t *testing.T) {
	dir := t.TempDir()
	name := artifactName("0.1.4", target{runtime.GOOS, runtime.GOARCH})
	if err := os.WriteFile(filepath.Join(dir, name), []byte("tampered executable"), 0755); err != nil {
		t.Fatal(err)
	}
	info := manifest{
		SchemaVersion: 1,
		Version:       "0.1.4",
		Artifacts: []artifact{{
			Target: runtime.GOOS + "/" + runtime.GOARCH,
			File:   name,
			SHA256: strings.Repeat("0", 64),
		}},
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := smokeExistingArtifact(dir); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered artifact was not rejected before execution: %v", err)
	}
}

// GL-072: a failure after the staging directory exists (here, a missing Go
// toolchain) leaves no .gitlocal-release-* directory behind; an existing
// artifact directory is refused before anything is staged.
func TestReleaseFailureRemovesStaging(t *testing.T) {
	parent := t.TempDir()
	out := filepath.Join(parent, "v-test")
	err := run("..", []string{"-host-only", "-out", out, "-go", filepath.Join(parent, "missing-go")})
	if err == nil || !strings.Contains(err.Error(), "read Go version") {
		t.Fatalf("release with a missing Go toolchain: %v", err)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed release left %v", entries)
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := run("..", []string{"-host-only", "-out", out}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("release over an existing directory: %v", err)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Fatalf("refused release left %v", entries)
	}
}
