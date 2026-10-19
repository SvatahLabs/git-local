package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GL-139: once the private store's loose objects pass the limit at which a
// snapshot packs it (GL-136's 6,700 objects), status and log say so on
// stderr; below it they print nothing. They never pack, and --json and
// --porcelain output stay the same.
func TestReadOnlyCommandsHintWhenTheStoreNeedsPacking(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	successful(t, repo.Root, "commit", "-m", "notes")
	run := func(args ...string) (string, string) {
		t.Helper()
		code, out, diagnostic := callCLI(t, repo.Root, args...)
		if code != 0 {
			t.Fatalf("git local %v: exit %d: %s", args, code, diagnostic)
		}
		return out, diagnostic
	}
	commands := [][]string{{"status"}, {"log"}, {"status", "--json"}, {"log", "--json"}, {"status", "--porcelain", "-z"}}
	before := map[string]string{}
	for _, args := range commands {
		out, diagnostic := run(args...)
		if diagnostic != "" {
			t.Fatalf("git local %v below the limit printed %q", args, diagnostic)
		}
		before[strings.Join(args, " ")] = out
	}
	// Loose objects are only counted, by name, so empty files stand in.
	objects := filepath.Join(repo.Root, ".git", "gitlocal", "history.git", "objects")
	packs := func() string {
		t.Helper()
		names, _ := filepath.Glob(filepath.Join(objects, "pack", "*"))
		return strings.Join(names, "\n")
	}
	packed := packs()
	for i := range 6700 {
		dir := filepath.Join(objects, fmt.Sprintf("%02x", i%256))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%038x", 1<<40+i)), nil, 0400); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range commands {
		out, diagnostic := run(args...)
		if out != before[strings.Join(args, " ")] {
			t.Fatalf("git local %v output changed past the limit:\n%s\nwas:\n%s", args, out, before[strings.Join(args, " ")])
		}
		json := args[len(args)-1] == "--json"
		if hinted := strings.Contains(diagnostic, "git local maintenance"); hinted == json || strings.Count(diagnostic, "\n") > 1 {
			t.Fatalf("git local %v past the limit printed %q", args, diagnostic)
		}
	}
	if now := packs(); now != packed {
		t.Fatalf("a read-only command packed the store:\n%s\nwas:\n%s", now, packed)
	}
}
