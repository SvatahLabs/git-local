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
