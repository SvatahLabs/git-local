package gitexec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

func newRunner(t *testing.T, repo *testutil.Repo) *Runner {
	t.Helper()
	r, err := New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func TestRejectInjectedContexts(t *testing.T) {
	repo := testutil.New(t)
	for _, key := range []string{"GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG", "GIT_CONFIG_GLOBAL"} {
		t.Run(key, func(t *testing.T) {
			if r, err := New(append(repo.RunnerEnv(), key+"=hostile")); err == nil {
				r.Close()
				t.Fatal("accepted override")
			}
		})
	}
}
func TestMainAndHistoryIsolation(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	r := newRunner(t, repo)
	ctx := context.Background()
	admin := filepath.Join(repo.Root, ".git")
	beforeRefs := repo.Git("show-ref")
	if _, err := r.Main(ctx, repo.Root, admin, "add", "app.txt"); err == nil {
		t.Fatal("main write allowed")
	}
	if _, err := r.Main(ctx, repo.Root, admin, "worktree", "remove", repo.Root); err == nil {
		t.Fatal("worktree mutation allowed")
	}
	if _, err := r.Main(ctx, repo.Root, admin, "config", "core.bare", "true"); err == nil {
		t.Fatal("config write allowed")
	}
	if _, err := r.Main(ctx, repo.Root, admin, "remote", "add", "backup", "https://example.com/repo.git"); err == nil {
		t.Fatal("remote mutation allowed")
	}
	if _, err := r.Main(ctx, repo.Root, admin, "check-ref-format", "refs/heads/feature/query"); err != nil {
		t.Fatal("branch ref validation was blocked", err)
	}
	if _, err := r.Main(ctx, repo.Root, admin, "check-ref-format", "--normalize", "refs/heads/main"); err == nil {
		t.Fatal("unrestricted ref validation flags were allowed")
	}
	if _, err := r.Main(ctx, repo.Root, admin, "check-ref-format", "refs/tags/v1"); err == nil {
		t.Fatal("non-branch ref validation was allowed")
	}
	marker := filepath.Join(repo.Root, "executed")
	repo.Git("config", "core.fsmonitor", testutil.HostileCommand(t, "TestHostileGitProgram", marker))
	repo.Git("status", "--short")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fsmonitor control did not execute: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Main(ctx, repo.Root, admin, "ls-files", "--stage", "-z"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("fsmonitor executed")
	}
	beforeIndex, _ := os.ReadFile(filepath.Join(admin, "index"))
	store := filepath.Join(repo.Root, "history.git")
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	raw := []byte("private\r\n\x00\xff")
	blob, err := r.History(ctx, store, bytes.NewReader(raw), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.History(ctx, store, nil, "cat-file", "blob", strings.TrimSpace(string(blob)))
	if err != nil || !bytes.Equal(raw, b) {
		t.Fatal("raw bytes changed", err)
	}
	if _, err = r.Main(ctx, repo.Root, admin, "cat-file", "-e", strings.TrimSpace(string(blob))); err == nil {
		t.Fatal("local blob leaked into main")
	}
	after, _ := os.ReadFile(filepath.Join(admin, "index"))
	if !bytes.Equal(beforeIndex, after) || !bytes.Equal(beforeRefs, repo.Git("show-ref")) {
		t.Fatal("main state changed")
	}
}
func TestReplacementRefsIgnored(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	head := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	tree := strings.TrimSpace(string(repo.Git("mktree")))
	replacement := strings.TrimSpace(string(repo.Git("commit-tree", tree, "-m", "replacement")))
	repo.Git("replace", head, replacement)
	r := newRunner(t, repo)
	b, err := r.Main(context.Background(), repo.Root, filepath.Join(repo.Root, ".git"), "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil || !bytes.Contains(b, []byte("app.txt")) {
		t.Fatal("replacement hid ownership", err)
	}
}
func TestPrivateStoreRejectsRedirects(t *testing.T) {
	for _, kind := range []string{"config", "alternates", "symlink", "promisor"} {
		t.Run(kind, func(t *testing.T) {
			repo := testutil.New(t)
			r := newRunner(t, repo)
			store := filepath.Join(repo.Root, "history.git")
			if err := r.InitBare(context.Background(), store); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "config":
				f, _ := os.OpenFile(filepath.Join(store, "config"), os.O_APPEND|os.O_WRONLY, 0600)
				f.WriteString("[include]\npath = hostile\n")
				f.Close()
			case "alternates":
				os.WriteFile(filepath.Join(store, "objects", "info", "alternates"), []byte("other"), 0600)
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(store, "refs", "unsafe")); err != nil {
					t.Skip(err)
				}
			case "promisor":
				os.WriteFile(filepath.Join(store, "objects", "pack", "x.promisor"), nil, 0600)
			}
			if _, err := r.History(context.Background(), store, strings.NewReader("secret"), "hash-object", "-w", "--stdin"); err == nil {
				t.Fatal("accepted unsafe store")
			}
		})
	}
}
func TestCapabilitiesAndCancellation(t *testing.T) {
	repo := testutil.New(t)
	r := newRunner(t, repo)
	if err := r.CheckCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Main(ctx, repo.Root, filepath.Join(repo.Root, ".git"), "ls-files", "-z"); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestPartialCloneDoesNotFetchOrRunHelper(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	repo.Git("config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(repo.Root, "partial")
	repo.Git("clone", "-q", "--no-local", "--no-checkout", "--filter=tree:0", repo.Root, clone)
	repo.At(clone, "config", "protocol.hostile.allow", "always")
	repo.At(clone, "config", "remote.origin.url", "hostile::never")
	r := newRunner(t, repo)
	before := repo.At(clone, "count-objects", "-v")
	if _, err := r.Main(context.Background(), clone, filepath.Join(clone, ".git"), "ls-tree", "-r", "HEAD"); err == nil {
		t.Fatal("missing tree treated as available")
	}
	if !bytes.Equal(before, repo.At(clone, "count-objects", "-v")) {
		t.Fatal("offline read changed object storage")
	}
}
func TestHistoryReferenceHookAndGlobalConfigDoNotExecute(t *testing.T) {
	repo := testutil.New(t)
	r := newRunner(t, repo)
	store := filepath.Join(repo.Root, "history.git")
	ctx := context.Background()
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(store, "executed")
	command := testutil.HostileCommand(t, "TestHostileGitProgram", marker)
	if err := os.WriteFile(filepath.Join(store, "hooks", "reference-transaction"), []byte("#!/bin/sh\n"+command+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// Both the store's default hook location and a global override are hostile.
	global := filepath.Join(repo.Root, "fixture-home", ".gitconfig")
	if err := os.WriteFile(global, []byte("[core]\n hooksPath = "+filepath.ToSlash(filepath.Join(store, "hooks"))+"\n[commit]\n gpgSign = true\n[gpg]\n program = must-not-execute\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tree, err := r.History(ctx, store, strings.NewReader(""), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := r.History(ctx, store, strings.NewReader("message\n"), "commit-tree", strings.TrimSpace(string(tree)))
	if err != nil {
		t.Fatal(err)
	}
	repo.Git("--git-dir="+store, "update-ref", "refs/controls/probe", strings.TrimSpace(string(commit)))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reference hook control did not execute: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	repo.Git("--git-dir="+store, "update-ref", "-d", "refs/controls/probe")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reference hook cleanup did not execute: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, err = r.History(ctx, store, nil, "update-ref", "refs/test", strings.TrimSpace(string(commit))); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(store, "executed")); !os.IsNotExist(err) {
		t.Fatal("reference hook executed")
	}
	if _, err = r.Main(ctx, repo.Root, filepath.Join(repo.Root, ".git"), "symbolic-ref", "HEAD", "refs/heads/changed"); err == nil {
		t.Fatal("symbolic-ref mutation allowed")
	}
}

func TestHostileGitProgram(t *testing.T) {
	testutil.RunHostileProgram()
}

func TestVersionFloor(t *testing.T) {
	for _, tc := range []struct {
		version string
		ok      bool
	}{{"git version 2.50.1 (Apple Git-155)", true}, {"git version 2.50.1.windows.1", true}, {"git version 2.51.0", true}, {"git version 2.50.0", false}, {"git version 2.49.2", false}, {"git version unknown", false}, {"git version 2.x.1", false}} {
		if err := requireVersion([]byte(tc.version)); (err == nil) != tc.ok {
			t.Fatalf("%q: %v", tc.version, err)
		}
	}
}
