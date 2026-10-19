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
	if !strings.Contains(out, "sensitive paths .env.local stay ignored but are no longer guarded") {
		t.Fatalf("deinit doesn't warn that the sensitive paths lost their guard:\n%s", out)
	}
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
	if strings.Contains(string(kept), "git-local managed") || !strings.HasPrefix(string(kept), "# kept by git local deinit") || !strings.Contains(string(kept), "\n# end of lines kept by git local deinit\n") {
		t.Fatalf("info/exclude doesn't replace the managed block by kept lines:\n%s", kept)
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

// GL-130 review F1: deinit --unignore removes the lines an earlier deinit
// kept, also when that deinit already set the state aside or was interrupted
// after it rewrote info/exclude, and says so, with the sensitive paths.
func TestUnignoreRemovesKeptLines(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	exclude := filepath.Join(root, ".git", "info", "exclude")
	identity := filepath.Join(root, ".git", "gitlocal-workspace.json")
	write(t, filepath.Join(root, ".env.local"), "TOKEN=secret\n")
	f.gl(root, "init")
	f.gl(root, "track", ".env.local")
	f.gl(root, "encrypt", "keygen", f.key)
	f.gl(root, "encrypt", "enable", "--key-file", f.key, "--", ".env.local")
	f.gl(root, "commit", "-m", "local files")
	savedIdentity, err := os.ReadFile(identity)
	if err != nil {
		t.Fatal(err)
	}
	f.gl(root, "deinit")
	untracked := func(when string) {
		t.Helper()
		if data, _ := os.ReadFile(exclude); string(data) != "/fixture-home/\n" {
			t.Fatalf("%s: deinit --unignore left ignore lines:\n%s", when, data)
		}
		if status := f.git(f.plain, root, "status", "--porcelain"); !strings.Contains(status, "?? .env.local") {
			t.Fatalf("%s: status lacks .env.local:\n%s", when, status)
		}
	}

	// Interrupted after the exclude write: the state and identity are back.
	archives, _ := filepath.Glob(filepath.Join(root, ".git", "gitlocal-removed-*"))
	if len(archives) != 1 {
		t.Fatalf("kept history: %q", archives)
	}
	if err := os.Rename(archives[0], filepath.Join(root, ".git", "gitlocal")); err != nil {
		t.Fatal(err)
	}
	write(t, identity, string(savedIdentity))
	out := f.gl(root, "deinit", "--unignore")
	if !strings.Contains(out, "Removed the lines an earlier git local deinit kept") || !strings.Contains(out, "sensitive paths .env.local are no longer ignored") {
		t.Fatalf("an interrupted deinit rerun with --unignore doesn't say it removed the kept lines:\n%s", out)
	}
	untracked("rerun of an interrupted deinit")

	// The state already set aside.
	f.gl(root, "init")
	f.gl(root, "track", ".env.local")
	f.gl(root, "encrypt", "enable", "--key-file", f.key, "--", ".env.local")
	f.gl(root, "deinit")
	out = f.gl(root, "deinit", "--unignore")
	if !strings.Contains(out, "Removed the lines an earlier git local deinit kept") || !strings.Contains(out, "sensitive paths .env.local are no longer ignored") {
		t.Fatalf("deinit --unignore after deinit doesn't say it removed the kept lines:\n%s", out)
	}
	untracked("deinit --unignore after deinit")
	if out = f.gl(root, "deinit", "--unignore"); strings.Contains(out, "Removed the") || strings.Contains(out, "sensitive") {
		t.Fatalf("deinit --unignore with nothing left to remove claims a removal:\n%s", out)
	}
}

// GL-130 review F2, F4: a path tracked again takes its kept line back into
// the managed block, so untracking it later stops ignoring it, and repeated
// init, track and deinit cycles keep no line twice.
func TestInitTakesBackKeptLines(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	exclude := filepath.Join(root, ".git", "info", "exclude")
	if err := os.Mkdir(filepath.Join(root, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "private", "notes.txt"), "notes\n")
	write(t, filepath.Join(root, "private", "keep.txt"), "keep\n")
	write(t, filepath.Join(root, "other.txt"), "other\n")
	f.gl(root, "init")
	f.gl(root, "track", "private")
	f.gl(root, "deinit")
	f.gl(root, "init")
	f.gl(root, "track", "private")
	if data, _ := os.ReadFile(exclude); strings.Contains(string(data), "kept by git local deinit") {
		t.Fatalf("tracking the kept path again left its kept line:\n%s", data)
	}
	f.gl(root, "untrack", "private/notes.txt")
	if _, err := f.run(f.plain, root, f.repo.GitPath, "check-ignore", "-q", "private/notes.txt"); err == nil {
		t.Fatal("a path untracked after init is still ignored by lines an earlier deinit kept")
	}
	f.gl(root, "track", "other.txt")
	f.gl(root, "deinit")
	data, _ := os.ReadFile(exclude)
	lines := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if lines[line] {
			t.Fatalf("init, track and deinit cycles kept %q twice:\n%s", line, data)
		}
		lines[line] = true
	}
	if _, err := f.run(f.plain, root, f.repo.GitPath, "check-ignore", "-q", "other.txt"); err != nil {
		t.Fatalf("other.txt isn't ignored after deinit:\n%s", data)
	}
}

// GL-130 review: init doesn't stop ignoring a path a deinit kept: it stays
// ignored until it's tracked again or deinit --unignore removes it.
func TestInitKeepsKeptPathsIgnored(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	write(t, filepath.Join(root, ".env"), "TOKEN=secret\n")
	f.gl(root, "init")
	f.gl(root, "track", ".env")
	f.gl(root, "deinit")
	if out := f.gl(root, "init"); strings.Contains(out, "kept") {
		t.Fatalf("init reports removing kept lines:\n%s", out)
	}
	if _, err := f.run(f.plain, root, f.repo.GitPath, "check-ignore", "-q", ".env"); err != nil {
		data, _ := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
		t.Fatalf(".env isn't ignored after deinit and init:\n%s", data)
	}
	f.git(f.plain, root, "add", "-A")
	if staged := f.git(f.plain, root, "diff", "--cached", "--name-only"); strings.Contains(staged, ".env") {
		t.Fatalf("git add -A after deinit and init staged:\n%s", staged)
	}
}

// GL-130 review F3: a staging file an abandoned seed left beside a selected
// file stays ignored after deinit.
func TestKeptLinesIgnoreStageFiles(t *testing.T) {
	f := newFixture(t)
	root := f.repo.Root
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "dir", "f"), "local\n")
	f.gl(root, "init")
	f.gl(root, "track", "dir/f")
	write(t, filepath.Join(root, "dir", ".gitlocal-tmp-0123"), "decrypted\n")
	f.gl(root, "deinit")
	if status := f.git(f.plain, root, "status", "--porcelain", "--untracked-files=all"); strings.Contains(status, ".gitlocal-tmp-") {
		t.Fatalf("a staging file shows as untracked after deinit:\n%s", status)
	}
}
