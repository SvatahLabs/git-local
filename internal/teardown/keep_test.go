package teardown_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GL-130: deinit keeps the selected paths ignored by ordinary lines in
// info/exclude, so git add -A stages none of them and git clean -fd keeps
// them; --unignore removes them as before, with the sensitive-path warning.
// The kept lines are written once, also when deinit runs again after an
// interruption or after git local init set the repository up again.
func TestDeinitKeepsSelectedPathsIgnored(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, "app.txt"), "application\n")
	f.git(f.plain, root, "add", "app.txt")
	f.git(f.plain, root, "commit", "-qm", "initial")
	f.gl(root, "init")
	if err := os.Mkdir(filepath.Join(root, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "private", "notes.txt"), "private notes\n")
	write(t, filepath.Join(root, ".env.local"), "TOKEN=secret\n")
	f.gl(root, "track", "private", ".env.local")
	f.gl(root, "encrypt", "keygen", f.key)
	f.gl(root, "encrypt", "enable", "--key-file", f.key, "--", ".env.local")
	f.gl(root, "commit", "-m", "local files")
	exclude := filepath.Join(root, ".git", "info", "exclude")
	identity := filepath.Join(root, ".git", "gitlocal-workspace.json")
	savedIdentity, err := os.ReadFile(identity)
	if err != nil {
		t.Fatal(err)
	}

	out := f.gl(root, "deinit")
	if strings.Contains(out, "no longer ignored") || strings.Contains(out, "now show as untracked") {
		t.Fatalf("deinit says the selected files are no longer ignored:\n%s", out)
	}
	if !strings.Contains(out, "kept by git local deinit") {
		t.Fatalf("deinit doesn't say it kept the selected files ignored:\n%s", out)
	}
	if status := f.git(f.plain, root, "status", "--porcelain"); status != "" {
		t.Fatalf("status after deinit lists files:\n%s", status)
	}
	f.git(f.plain, root, "add", "-A")
	if staged := f.git(f.plain, root, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("git add -A after deinit staged:\n%s", staged)
	}
	f.git(f.plain, root, "clean", "-fdq")
	for _, name := range []string{"private/notes.txt", ".env.local"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("git clean -fd after deinit removed %s: %v", name, err)
		}
	}
	kept, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(kept), "git-local managed") || strings.Contains(string(kept), ".gitlocal-tmp-") {
		t.Fatalf("info/exclude still holds the managed block:\n%s", kept)
	}
	if !strings.HasSuffix(string(kept), "/fixture-home/\n") {
		t.Fatalf("the user's exclude lines changed:\n%s", kept)
	}

	// A deinit interrupted after it rewrote info/exclude, run again: the
	// private state and the identity are back, the block is gone.
	archives, _ := filepath.Glob(filepath.Join(root, ".git", "gitlocal-removed-*"))
	if len(archives) != 1 {
		t.Fatalf("kept history: %q", archives)
	}
	if err := os.Rename(archives[0], filepath.Join(root, ".git", "gitlocal")); err != nil {
		t.Fatal(err)
	}
	write(t, identity, string(savedIdentity))
	f.gl(root, "deinit")
	if again, _ := os.ReadFile(exclude); string(again) != string(kept) {
		t.Fatalf("a rerun changed info/exclude:\n%s\nwas:\n%s", again, kept)
	}

	// Set up again and removed again: the kept lines stay once.
	f.gl(root, "init")
	f.gl(root, "track", "private", ".env.local")
	f.gl(root, "deinit")
	if again, _ := os.ReadFile(exclude); string(again) != string(kept) {
		t.Fatalf("deinit after a new init wrote the kept lines again:\n%s\nwas:\n%s", again, kept)
	}

	// --unignore removes them from the ignore rules, as before.
	write(t, exclude, "/fixture-home/\n")
	f.gl(root, "init")
	f.gl(root, "track", "private", ".env.local")
	f.gl(root, "encrypt", "enable", "--key-file", f.key, "--", ".env.local")
	out = f.gl(root, "deinit", "--unignore")
	if !strings.Contains(out, "sensitive paths .env.local are no longer ignored") || !strings.Contains(out, "now show as untracked") {
		t.Fatalf("deinit --unignore doesn't warn that the files are no longer ignored:\n%s", out)
	}
	if data, _ := os.ReadFile(exclude); string(data) != "/fixture-home/\n" {
		t.Fatalf("deinit --unignore left ignore lines:\n%s", data)
	}
	status := f.git(f.plain, root, "status", "--porcelain")
	for _, want := range []string{"?? private/", "?? .env.local"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status after deinit --unignore lacks %q:\n%s", want, status)
		}
	}
}
