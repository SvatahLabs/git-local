package overlay

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFilterConfigUsesOneLocalInclude(t *testing.T) {
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	executable := filepath.Join(root, "my filter's binary")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	data, err := FilterConfig(executable)
	if err != nil {
		t.Fatal(err)
	}
	include := filepath.Join(root, ".git", "overlay-filter.gitconfig")
	if err := os.WriteFile(include, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(git, "config", "--local", "--add", "include.path", include)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("include: %v %s", err, out)
	}
	cmd = exec.Command(git, "config", "--get", "filter.gitlocal-overlay.process")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != filterCommandForOS(executable, runtime.GOOS) {
		t.Fatalf("unexpected command: %v %q", err, out)
	}
	cmd = exec.Command(git, "config", "--bool", "--get", "filter.gitlocal-overlay.required")
	cmd.Dir = root
	out, err = cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("filter is not required: %v %q", err, out)
	}
}

func TestInstalledCommandRequiresGeneratedFilterConfig(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "git-local's filter")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	config, err := FilterConfig(executable)
	if err != nil {
		t.Fatal(err)
	}
	path, command, err := installedCommand(config)
	wantPath := executable
	if runtime.GOOS == "windows" {
		wantPath = filepath.ToSlash(executable)
	}
	if err != nil || path != wantPath || command != filterCommandForOS(executable, runtime.GOOS) {
		t.Fatalf("generated command did not round-trip: %q %q %v", path, command, err)
	}
	if _, _, err := installedCommand(append(config, []byte("[filter \"gitlocal-overlay\"]\n\tprocess = cat\n")...)); err == nil {
		t.Fatal("accepted appended process override")
	}
}

func TestWindowsFilterCommandUsesShellSafeDrivePath(t *testing.T) {
	command := filterCommandForOS(`C:\Program Files\Atul's GitLocal\git-local.exe`, "windows")
	want := `'C:/Program Files/Atul'\''s GitLocal/git-local.exe' overlay filter-process`
	if command != want {
		t.Fatalf("Windows filter command: %q, want %q", command, want)
	}
}

func TestWindowsExecutableAuditAcceptsExeWithoutPOSIXModeBits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "git-local.exe")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if !filterExecutableReadyForOS(path, "windows") || filterExecutableReadyForOS(path, "linux") {
		t.Fatal("Windows executable audit must use .exe instead of POSIX execute bits")
	}
}

func TestFilterConfigUsesGitConfigQuoting(t *testing.T) {
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	executable := filepath.Join(root, "Tools\u00a0dir \u00e9t\u00e9\u3000x", `back\slash "quoted"`, "git-local")
	if runtime.GOOS == "windows" {
		executable = filepath.Join(root, "Tools\u00a0dir \u00e9t\u00e9\u3000x", "git-local.exe")
	}
	data, err := FilterConfig(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", filterConfigName), data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(git, "config", "--local", "--add", "include.path", filterConfigName)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("include: %v %s", err, out)
	}
	cmd = exec.Command(git, "config", "--list")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Git cannot parse the filter config: %v %s\n%s", err, out, data)
	}
	cmd = exec.Command(git, "config", "--get", "filter.gitlocal-overlay.process")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSuffix(string(out), "\n") != filterCommandForOS(executable, runtime.GOOS) {
		t.Fatalf("process did not round-trip through Git: %v %q", err, out)
	}
	if _, command, err := installedCommand(data); err != nil || command != filterCommandForOS(executable, runtime.GOOS) {
		t.Fatalf("installed command did not round-trip: %v %q", err, command)
	}
	for _, bad := range []string{"tab\there", "bell\x07", "del\x7f"} {
		if _, err := FilterConfig(filepath.Join(root, bad, "git-local")); err == nil || !strings.Contains(err.Error(), "control character") {
			t.Fatalf("accepted control character in %q: %v", bad, err)
		}
	}
}
