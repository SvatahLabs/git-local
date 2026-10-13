package selection

import (
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// MainState starts no Git process, yet shows every way Git moves HEAD or
// changes the index: a commit, update-ref of a loose or packed ref, a
// detached HEAD, a branch switch, and an index rewrite.
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
