package cli

import (
	"strings"
	"testing"
)

// The completion scripts call the hidden __complete command; it must reach
// Cobra instead of failing the unknown-command check.
func TestShellCompletionRequestsListSubcommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, request := range []string{"__complete", "__completeNoDesc"} {
		code, out, diagnostic := callCLI(t, dir, request, "hook", "")
		if code != 0 || !strings.Contains(out, "install") || !strings.Contains(out, "uninstall") {
			t.Fatalf("%s hook: exit %d\n%s\n%s", request, code, out, diagnostic)
		}
	}
	code, out, _ := callCLI(t, dir, "completion", "bash")
	if code != 0 || !strings.Contains(out, "__complete") {
		t.Fatalf("completion bash: exit %d %q", code, out)
	}
	if code, _, diagnostic := callCLI(t, dir, "help", "bogus"); code != 2 || !strings.Contains(diagnostic, "bogus") {
		t.Fatalf("help bogus: exit %d %s", code, diagnostic)
	}
	if code, out, _ := callCLI(t, dir, "help", "hook"); code != 0 || !strings.Contains(out, "install") {
		t.Fatalf("help hook: exit %d %q", code, out)
	}
	for _, args := range [][]string{{"help", "hook", "bogus"}, {"--json", "help", "bogus"}} {
		if code, out, diagnostic := callCLI(t, dir, args...); code != 2 || !strings.Contains(out+diagnostic, "bogus") {
			t.Fatalf("%v: exit %d %s %s", args, code, out, diagnostic)
		}
	}
}

// A command line being completed may already hold --json; the shell still
// gets completion lines and a directive, not a JSON envelope.
func TestShellCompletionIgnoresJSONFlagBeingCompleted(t *testing.T) {
	t.Parallel()
	code, out, diagnostic := callCLI(t, t.TempDir(), "__complete", "hook", "--json", "")
	if code != 0 || strings.Contains(out, "schemaVersion") || !strings.Contains(out, "install") || !strings.Contains(out, ":") {
		t.Fatalf("__complete hook --json: exit %d\n%s\n%s", code, out, diagnostic)
	}
}
