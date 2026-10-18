package selection

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// With the reftable backend refs/heads is a file, so the loose ref MainState
// stats is "not a directory" (which stopped every restore and seed), and a
// linked worktree keeps its HEAD in its own stack: a detached HEAD moved
// there changes neither the HEAD file, the index nor the repository's stack.
func TestMainStateReadsReftableRepositories(t *testing.T) {
	repo := testutil.NewReftable(t)
	repo.Commit()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(parent, "linked")
	repo.Git("worktree", "add", "-q", "--detach", linked)
	common := filepath.Join(repo.Root, ".git")
	for _, c := range []struct{ name, root, admin string }{
		{"main worktree", repo.Root, common},
		{"linked worktree", linked, filepath.Join(common, "worktrees", "linked")},
	} {
		read := func() MainState {
			t.Helper()
			s, err := ReadMainState(c.admin, common, filepath.Join(c.admin, "index"))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			return s
		}
		before := read()
		if !before.Same(read()) || !before.Settled() {
			t.Fatalf("%s: unchanged state differs or isn't settled", c.name)
		}
		next := strings.TrimSpace(string(repo.At(c.root, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "next")))
		repo.At(c.root, "update-ref", "--no-deref", "HEAD", next)
		if read().Same(before) {
			t.Fatalf("%s: HEAD moved unseen", c.name)
		}
	}
}

// A HEAD MainState doesn't follow is never the same as another reading, so
// ownership is listed again: a symbolic ref chain (HEAD names a ref that names
// main, so a commit on main moves HEAD and changes none of the files read)
// and HEAD spelled without the space after "ref:", which Git accepts.
func TestMainStateDoesntTrustHeadsItDoesntFollow(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	git := filepath.Join(repo.Root, ".git")
	read := func() MainState {
		t.Helper()
		s, err := ReadMainState(git, git, filepath.Join(git, "index"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	repo.Git("symbolic-ref", "refs/heads/alias", "refs/heads/main")
	repo.Git("symbolic-ref", "HEAD", "refs/heads/alias")
	if s := read(); s.Same(s) || s.Settled() {
		t.Error("a symbolic ref chain is trusted")
	}
	if err := os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref:refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(repo.Git("symbolic-ref", "HEAD"))); got != "refs/heads/main" {
		t.Fatalf("Git reads the HEAD as %q", got)
	}
	if s := read(); s.Same(s) || s.Settled() {
		t.Fatal("a HEAD spelled \"ref:refs/...\" is trusted")
	}
}

// MainState starts no Git process, yet shows every way Git moves HEAD or
// changes the index: a commit, update-ref of a loose or packed ref, a
// detached HEAD, a branch switch, a configuration edit (it can change the ref
// backend), and an index rewrite.
func TestMainStateShowsHeadAndIndexChanges(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	admin, common, index := filepath.Join(repo.Root, ".git"), filepath.Join(repo.Root, ".git"), filepath.Join(repo.Root, ".git", "index")
	read := func() MainState {
		t.Helper()
		s, err := ReadMainState(admin, common, index)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	commit := func() string {
		return strings.TrimSpace(string(repo.Git("commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "next")))
	}
	for _, change := range []struct {
		name string
		run  func()
	}{
		{"commit", func() { repo.Git("commit", "-q", "--allow-empty", "-m", "next") }},
		{"update-ref", func() { repo.Git("update-ref", "refs/heads/main", commit()) }},
		{"packed update-ref", func() {
			repo.Git("pack-refs", "--all")
			before := read()
			repo.Git("update-ref", "refs/heads/main", commit())
			if read().Same(before) {
				t.Fatal("update of a packed ref unseen")
			}
		}},
		{"detach", func() { repo.Git("checkout", "-q", "--detach") }},
		{"detached move", func() { repo.Git("update-ref", "--no-deref", "HEAD", commit()) }},
		{"switch", func() { repo.Git("checkout", "-q", "-b", "other") }},
		{"config", func() { repo.Git("config", "gitlocal.test", "true") }},
		{"index", func() {
			repo.Git("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(string(repo.Git("hash-object", "-w", "app.txt")))+",new.txt")
		}},
	} {
		before := read()
		if !before.Same(read()) || !before.Settled() {
			t.Fatalf("%s: unchanged state differs or isn't settled", change.name)
		}
		change.run()
		if read().Same(before) {
			t.Fatalf("%s: change unseen", change.name)
		}
	}
}

// RefFiles starts no Git process, yet shows every way Git changes a ref of a
// directory no worktree checks out: update-ref of a loose or packed ref,
// packing, deletion and a configuration edit; a ref holding a symbolic ref is
// never the same as another reading (GL-127).
func TestRefFilesShowRefChanges(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	git := filepath.Join(repo.Root, ".git")
	read := func() RefFiles {
		t.Helper()
		s, err := ReadRefFiles(git, "refs/heads/kept", "refs/heads/other")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	n := 0
	commit := func() string {
		n++
		return strings.TrimSpace(string(repo.Git("commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", fmt.Sprintf("next %d", n))))
	}
	repo.Git("branch", "kept")
	for _, change := range []struct {
		name string
		run  func()
	}{
		{"update-ref", func() { repo.Git("update-ref", "refs/heads/kept", commit()) }},
		{"create", func() { repo.Git("update-ref", "refs/heads/other", commit()) }},
		{"pack", func() { repo.Git("pack-refs", "--all") }},
		{"packed update-ref", func() { repo.Git("update-ref", "--no-deref", "refs/heads/other", commit()) }},
		{"delete", func() { repo.Git("update-ref", "-d", "refs/heads/other") }},
		{"config", func() { repo.Git("config", "gitlocal.test", "true") }},
		{"symbolic", func() { repo.Git("symbolic-ref", "refs/heads/other", "refs/heads/kept") }},
	} {
		before := read()
		if !before.Same(read()) {
			t.Fatalf("%s: unchanged state differs", change.name)
		}
		change.run()
		if read().Same(before) {
			t.Fatalf("%s: change unseen", change.name)
		}
	}
	if s := read(); s.Same(s) || s.Settled() {
		t.Fatal("a symbolic ref is trusted")
	}
}
