package cli

import (
	"strings"
	"testing"
)

// The completion scripts call the hidden __complete command; it must reach
// Cobra instead of failing the unknown-command check.
func TestShellCompletionRequestsListSubcommands(t *testing.T) {
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
}
