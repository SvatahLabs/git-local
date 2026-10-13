package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestReleaseInterruptHelper runs the builder as main does, with a Go
// toolchain that hangs, for TestReleaseInterruptRemovesStaging.
func TestReleaseInterruptHelper(t *testing.T) {
	out := os.Getenv("GITLOCAL_TEST_RELEASE_OUT")
	if out == "" {
		t.Skip("subprocess helper")
	}
	stopOnInterrupt()
	err := run("..", []string{"-host-only", "-out", out, "-go", os.Getenv("GITLOCAL_TEST_RELEASE_GO")})
	if interrupted.Err() != nil {
		os.Exit(130) // As main does, after run removed the staging directory.
	}
	t.Fatalf("release builder wasn't interrupted: %v", err)
}

// GL-115: an interrupted release build removes its staging directory too.
func TestReleaseInterruptRemovesStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGINT on Windows")
	}
	parent := t.TempDir()
	hanging := filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(hanging, []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReleaseInterruptHelper$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), "GITLOCAL_TEST_RELEASE_OUT="+filepath.Join(parent, "v-test"), "GITLOCAL_TEST_RELEASE_GO="+hanging)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		if entries, _ := os.ReadDir(parent); len(entries) == 1 && strings.HasPrefix(entries[0].Name(), ".gitlocal-release-") {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the builder never staged")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 130 {
		t.Fatalf("interrupted builder: %v", err)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Fatalf("interrupted release left %v", entries)
	}
}
