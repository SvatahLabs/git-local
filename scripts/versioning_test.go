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
	if !strings.Contains(string(readme), "Git "+gitexec.MinimumGit+" or later") {
		t.Fatalf("README doesn't name the minimum Git version %s", gitexec.MinimumGit)
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
	if out, err := exec.Command("bash", "build-macos-installer.sh", version).CombinedOutput(); err == nil || !strings.Contains(string(out), "VERSION") {
		t.Fatalf("installer accepted a version argument: %v\n%s", err, out)
	}
}
