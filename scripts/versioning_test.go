package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
)

// GL-060: the version lives in VERSION and the minimum Git version in
// gitexec.MinimumGit; scripts, CI and docs don't repeat either, except where
// a check here keeps the copy equal.
func TestVersionAndMinimumGitAreSingleSourced(t *testing.T) {
	version, err := readVersion("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release-binaries.go", "build-macos-installer.sh", "../.github/workflows/ci.yml"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(version) + `\b|0\.1\.4`).Match(data) {
			t.Fatalf("%s hard-codes a release version", name)
		}
		if name != "../.github/workflows/ci.yml" && strings.Contains(string(data), gitexec.MinimumGit) {
			t.Fatalf("%s hard-codes the minimum Git version", name)
		}
	}
	// CI must build and test the minimum Git version itself.
	ci, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ci), "gitVersion: '"+gitexec.MinimumGit+"'") {
		t.Fatalf("CI doesn't test the minimum Git version %s", gitexec.MinimumGit)
	}
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`Git\s+` + regexp.QuoteMeta(gitexec.MinimumGit) + `\s+or\s+later`).Match(readme) {
		t.Fatalf("README doesn't name the minimum Git version %s", gitexec.MinimumGit)
	}
	// The user docs repeat both for readers; every copy must be current.
	docs, err := filepath.Glob("../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	// Markdown wraps lines, so a phrase may span a line break.
	minimum := regexp.MustCompile(`Git\s+(\d+\.\d+\.\d+)\s+or\s+later`)
	artifacts := regexp.MustCompile(`git-local_v(\d+\.\d+\.\d+)_|GitLocal-(\d+\.\d+\.\d+)-`)
	for _, name := range append(docs, "../README.md") {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range minimum.FindAllStringSubmatch(string(data), -1) {
			if match[1] != gitexec.MinimumGit {
				t.Errorf("%s names Git %s as the minimum; it's %s", name, match[1], gitexec.MinimumGit)
			}
		}
		for _, match := range artifacts.FindAllStringSubmatch(string(data), -1) {
			if found := match[1] + match[2]; found != version {
				t.Errorf("%s names %s artifacts; VERSION is %s", name, found, version)
			}
		}
	}
	for _, dir := range []string{"../internal", "../cmd"} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, filepath.Join("gitexec", "runner.go")) {
				return err
			}
			data, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(data), gitexec.MinimumGit) {
				t.Errorf("%s hard-codes the minimum Git version", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadVersionRefusesMalformedVersions(t *testing.T) {
	for _, content := range []string{"", "1.0", "v1.0.0", "1.0.0-rc1", "1.0.0\n2.0.0\n"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := readVersion(dir); err == nil {
			t.Fatalf("VERSION %q read as %q", content, got)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("2.3.4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readVersion(dir); err != nil || got != "2.3.4" {
		t.Fatalf("VERSION 2.3.4: %q %v", got, err)
	}
}

// The installer script refuses to replace a package that already exists for
// this version, before building anything.
func TestMacOSInstallerRefusesToOverwrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the installer builds only on macOS")
	}
	version, err := readVersion("..")
	if err != nil {
		t.Fatal(err)
	}
	dist := t.TempDir()
	existing := filepath.Join(dist, "GitLocal-"+version+"-macos-universal.pkg")
	if err := os.WriteFile(existing, []byte("released package"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "build-macos-installer.sh")
	cmd.Env = append(os.Environ(), "GITLOCAL_DIST_DIR="+dist, "GITLOCAL_GO_BIN=false")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refusing to overwrite") {
		t.Fatalf("installer over an existing package: %v\n%s", err, out)
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "released package" {
		t.Fatalf("existing package changed: %q %v", data, err)
	}
	if extra, _ := filepath.Glob(filepath.Join(dist, "*")); len(extra) != 1 {
		t.Fatalf("installer wrote %q", extra)
	}
	// Even if the argument check regressed, nothing is built or written.
	withArgument := exec.Command("bash", "build-macos-installer.sh", version)
	withArgument.Env = append(os.Environ(), "GITLOCAL_DIST_DIR="+t.TempDir(), "GITLOCAL_GO_BIN=false")
	if out, err := withArgument.CombinedOutput(); err == nil || !strings.Contains(string(out), "VERSION") {
		t.Fatalf("installer accepted a version argument: %v\n%s", err, out)
	}
}

// A check that fails after the package is built leaves nothing in the output
// directory, so it doesn't block the next run. It builds a real package, so
// it runs only on macOS, in a Git checkout of the source, and not in -short.
func TestMacOSInstallerFailureLeavesNoPackage(t *testing.T) {
	if runtime.GOOS != "darwin" || testing.Short() {
		t.Skip("builds a macOS package")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if top, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output(); err != nil || filepath.Clean(strings.TrimSpace(string(top))) != filepath.Clean(mustEvalSymlinks(t, root)) {
		t.Skip("not a Git checkout of the source")
	}
	// cmp compares the built and the packaged binary, after pkgbuild.
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "cmp"), []byte("#!/bin/sh\necho 'injected cmp failure' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dist := t.TempDir()
	cmd := exec.Command("bash", "build-macos-installer.sh")
	cmd.Env = append(os.Environ(), "GITLOCAL_DIST_DIR="+dist, "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "injected cmp failure") {
		t.Fatalf("installer didn't fail at the check after pkgbuild: %v\n%s", err, out)
	}
	if left, _ := filepath.Glob(filepath.Join(dist, "*")); len(left) != 0 {
		t.Fatalf("failed installer left %q", left)
	}
	if left, _ := filepath.Glob(filepath.Join(dist, ".*")); len(left) != 0 {
		t.Fatalf("failed installer left %q", left)
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
