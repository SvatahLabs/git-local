package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// completionBinary builds git-local (or copies GITLOCAL_TEST_BINARY) into an
// empty directory, for the shell scripts' __complete requests.
func completionBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "git-local")
	if prebuilt := os.Getenv("GITLOCAL_TEST_BINARY"); prebuilt != "" {
		source, err := os.Open(prebuilt)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		destination, err := os.OpenFile(bin, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(destination, source); err != nil {
			t.Fatal(err)
		}
		if err := destination.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, "../../cmd/git-local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build git-local: %v\n%s", err, output)
	}
	return dir
}

// shellEnv runs a shell with an isolated HOME and git-local first on PATH.
func shellEnv(t *testing.T, bin string) []string {
	t.Helper()
	home := t.TempDir()
	return []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "LC_ALL=C"}
}

func gitBashCompletion(t *testing.T) string {
	t.Helper()
	candidates := []string{"/usr/share/bash-completion/completions/git", "/etc/bash_completion.d/git", "/opt/homebrew/etc/bash_completion.d/git-completion.bash", "/usr/local/etc/bash_completion.d/git-completion.bash"}
	if out, err := exec.Command("git", "--exec-path").Output(); err == nil {
		execPath := strings.TrimSpace(string(out))
		candidates = append([]string{filepath.Join(execPath, "..", "..", "share", "git-core", "git-completion.bash"), filepath.Join(execPath, "..", "..", "share", "git-core", "contrib", "completion", "git-completion.bash")}, candidates...)
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	t.Skip("Git's bash completion script isn't installed")
	return ""
}

// GL-106: with Git's bash completion loaded, `git local <TAB>` reaches
// git-local, as `git-local <TAB>` always did.
func TestBashCompletionCompletesGitLocalForm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash completion isn't tested on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash isn't installed")
	}
	gitScript := gitBashCompletion(t)
	bin := completionBinary(t)
	code, script, _ := callCLI(t, t.TempDir(), "completion", "bash")
	if code != 0 {
		t.Fatalf("completion bash: exit %d", code)
	}
	scriptPath := filepath.Join(t.TempDir(), "git-local.bash")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	const driver = `
source "$1" || exit 3
source "$2" || exit 3
complete_line() {
	COMP_LINE="$1"; COMP_POINT=${#COMP_LINE}
	read -ra COMP_WORDS <<< "$1"
	[[ "$1" == *" " ]] && COMP_WORDS+=("")
	COMP_CWORD=$(( ${#COMP_WORDS[@]} - 1 ))
	COMPREPLY=()
	__git_wrap__git_main
	echo "== $1"
	printf '%s\n' "${COMPREPLY[@]}"
}
complete_line "git local "
complete_line "git local ho"
complete_line "git local hook "
complete_line "git-local hook "
`
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", driver, "driver", gitScript, scriptPath)
	cmd.Env = shellEnv(t, bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	sections := map[string][]string{}
	current := ""
	for _, line := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "== "); ok {
			current = name
			continue
		}
		if line = strings.TrimSpace(line); line != "" {
			sections[current] = append(sections[current], line)
		}
	}
	has := func(line string, want ...string) {
		t.Helper()
		for _, w := range want {
			found := false
			for _, got := range sections[line] {
				if got == w {
					found = true
				}
			}
			if !found {
				t.Fatalf("%q doesn't complete %q:\n%s", line, w, out)
			}
		}
	}
	has("git local ", "commit", "track", "hook", "restore")
	has("git local ho", "hook")
	has("git local hook ", "install", "uninstall")
	// The git-local form is unchanged; it uses its own complete registration.
	if len(sections["git-local hook "]) != 0 {
		t.Logf("git-local form under Git's wrapper: %q", sections["git-local hook "])
	}
}

// GL-106: zsh's _git completes `git local ...` through _git-local, with
// words[1] set to "local"; the request must still run git-local.
func TestZshCompletionCompletesGitLocalForm(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("zsh isn't installed")
	}
	bin := completionBinary(t)
	code, script, _ := callCLI(t, t.TempDir(), "completion", "zsh")
	if code != 0 {
		t.Fatalf("completion zsh: exit %d", code)
	}
	scriptPath := filepath.Join(t.TempDir(), "git-local.zsh")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	// compsys isn't loaded: stub what the script calls and print the
	// completions it would offer.
	const driver = `
compdef() { :; }
compadd() { :; }
_describe() { print -rl -- "${completions[@]}"; }
source "$1" || exit 3
complete_words() {
	local -a words; words=("$@"); local CURRENT=$#
	print -r -- "== ${words[*]}"
	_git-local
}
complete_words local ""
complete_words local hook ""
complete_words git-local hook ""
`
	cmd := exec.Command(zsh, "-f", "-c", driver, "driver", scriptPath)
	cmd.Env = shellEnv(t, bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("zsh: %v\n%s", err, out)
	}
	sections := map[string]string{}
	current := ""
	for _, line := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "== "); ok {
			current = name
			continue
		}
		sections[current] += line + "\n"
	}
	for line, want := range map[string][]string{"local ": {"commit:", "track:", "hook:"}, "local hook ": {"install:", "uninstall:"}, "git-local hook ": {"install:", "uninstall:"}} {
		for _, w := range want {
			if !strings.Contains(sections[line], "\n"+w) && !strings.HasPrefix(sections[line], w) {
				t.Fatalf("zsh %q doesn't complete %q:\n%s", line, w, out)
			}
		}
	}
}
