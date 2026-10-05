package hook_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestAutomaticSnapshotHooksCaptureGitEventsWithoutDuplicates(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	bin := testBinary(t)
	runLocal := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git-local %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	runLocal("init")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runLocal("track", "notes.local")
	if report := runLocal("doctor"); !strings.Contains(report, "automatic snapshots") {
		t.Fatalf("doctor did not flag missing automatic hooks: %s", report)
	}
	hooks := filepath.Join(repo.Root, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("#!/bin/sh\nprintf 'original post-commit\\n' >> original-hook.log\n")
	if err := os.WriteFile(filepath.Join(hooks, "post-commit"), original, 0700); err != nil {
		t.Fatal(err)
	}
	runLocal("hook", "install")
	runLocal("auto", "install")
	if report := runLocal("doctor"); strings.Contains(report, "automatic snapshots") {
		t.Fatalf("doctor still flagged installed automatic hooks: %s", report)
	}
	if status := runLocal("auto", "status"); !strings.Contains(status, "post-commit: installed") || !strings.Contains(status, "pre-rebase: installed") {
		t.Fatalf("automatic hook status: %s", status)
	}
	offline := bin + ".offline"
	if err := os.Rename(bin, offline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "offline.txt"), []byte("offline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "offline.txt")
	repo.Git("commit", "--no-verify", "-qm", "binary unavailable")
	if err := os.Rename(offline, bin); err != nil {
		t.Fatal(err)
	}
	if oldLog, err := os.ReadFile(filepath.Join(repo.Root, "original-hook.log")); err != nil || !bytes.Contains(oldLog, []byte("original post-commit")) {
		t.Fatalf("missing binary prevented existing post-commit hook: %v %q", err, oldLog)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.txt"), []byte("normal 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.txt")
	repo.Git("commit", "-qm", "mainline one")
	log := runLocal("log")
	if strings.Count(log, "Automatic local snapshot") != 1 || !strings.Contains(log, "post-commit") {
		t.Fatalf("post-commit did not snapshot selected file: %s", log)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.txt"), []byte("normal 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.txt")
	repo.Git("commit", "-qm", "mainline two")
	if log = runLocal("log"); strings.Count(log, "Automatic local snapshot") != 1 {
		t.Fatalf("unchanged selected file created duplicate snapshot: %s", log)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("checkout", "-q", "-b", "feature")
	if log = runLocal("log"); strings.Count(log, "Automatic local snapshot") != 2 || !strings.Contains(log, "post-checkout") {
		t.Fatalf("post-checkout did not snapshot changed note: %s", log)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "feature.txt")
	repo.Git("commit", "-qm", "feature")
	repo.Git("checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo.Root, "main.txt"), []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "main.txt")
	repo.Git("commit", "-qm", "main")
	repo.Git("checkout", "-q", "feature")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("third\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("rebase", "main")
	if log = runLocal("log"); strings.Count(log, "Automatic local snapshot") != 3 || !strings.Contains(log, "pre-rebase") {
		t.Fatalf("pre-rebase did not capture pending note: %s", log)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("fourth\n"), 0600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "linked", linked)
	if got, err := os.ReadFile(filepath.Join(linked, "notes.local")); err != nil || !bytes.Equal(got, []byte("fourth\n")) {
		t.Fatalf("new worktree did not inherit main selection and file: %v %q", err, got)
	}
	linkedStatus := exec.Command(bin, "status")
	linkedStatus.Dir, linkedStatus.Env = linked, repo.RunnerEnv()
	if out, err := linkedStatus.CombinedOutput(); err != nil {
		t.Fatalf("new worktree was not initialized: %v\n%s", err, out)
	}
	if log = runLocal("log"); strings.Count(log, "Automatic local snapshot") != 4 || !strings.Contains(log, "before worktree bootstrap") {
		t.Fatalf("worktree bootstrap did not capture latest main file: %s", log)
	}
	secondLinked := filepath.Join(t.TempDir(), "linked-two")
	repo.Git("worktree", "add", "-q", "-b", "linked-two", secondLinked)
	if got, err := os.ReadFile(filepath.Join(secondLinked, "notes.local")); err != nil || !bytes.Equal(got, []byte("fourth\n")) {
		t.Fatalf("second worktree did not inherit latest selected file: %v %q", err, got)
	}
	if log = runLocal("log"); strings.Count(log, "Automatic local snapshot") != 4 {
		t.Fatalf("second worktree creation duplicated main snapshot: %s", log)
	}
	secondAdmin := strings.TrimSpace(string(repo.At(secondLinked, "rev-parse", "--absolute-git-dir")))
	secondIdentity, err := os.ReadFile(filepath.Join(secondAdmin, "gitlocal-workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var secondWorkspace struct{ WorkspaceID string }
	if err := json.Unmarshal(secondIdentity, &secondWorkspace); err != nil {
		t.Fatal(err)
	}
	runLocal("workspace", "remove", secondLinked)
	if _, err := os.Stat(secondLinked); !os.IsNotExist(err) {
		t.Fatalf("workspace remove left linked checkout: %v", err)
	}
	if entries := runLocal("workspace", "list"); !strings.Contains(entries, secondWorkspace.WorkspaceID+" retained") {
		t.Fatalf("workspace remove did not retire linked checkout: %s", entries)
	}
	if retained := runLocal("log", "--workspace", secondWorkspace.WorkspaceID); !strings.Contains(retained, "before worktree removal") {
		t.Fatalf("workspace remove did not snapshot selected files: %s", retained)
	}
	linkedCommit := exec.Command(bin, "commit", "-m", "linked snapshot")
	linkedCommit.Dir, linkedCommit.Env = linked, repo.RunnerEnv()
	if out, err := linkedCommit.CombinedOutput(); err != nil {
		t.Fatalf("linked worktree snapshot failed: %v\n%s", err, out)
	}
	linkedAdmin := strings.TrimSpace(string(repo.At(linked, "rev-parse", "--absolute-git-dir")))
	identityData, err := os.ReadFile(filepath.Join(linkedAdmin, "gitlocal-workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var linkedIdentity struct{ WorkspaceID string }
	if err := json.Unmarshal(identityData, &linkedIdentity); err != nil {
		t.Fatal(err)
	}
	linkedID := linkedIdentity.WorkspaceID
	if linkedID == "" {
		t.Fatal("linked worktree has no workspace ID")
	}
	repo.Git("worktree", "remove", "--force", linked)
	if preview := runLocal("workspace", "prune", "--dry-run"); !strings.Contains(preview, linkedID) {
		t.Fatalf("removed worktree was not eligible for prune: %s", preview)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "retirement-trigger.txt"), []byte("trigger\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "retirement-trigger.txt")
	repo.Git("commit", "-qm", "trigger automatic worktree reconciliation")
	if entries := runLocal("workspace", "list"); !strings.Contains(entries, linkedID+" retained") {
		t.Fatalf("post-commit did not retain removed worktree: %s", entries)
	}
	runLocal("workspace", "prune")
	if entries := runLocal("workspace", "list"); !strings.Contains(entries, linkedID+" retained") {
		t.Fatalf("prune did not retain removed worktree: %s", entries)
	}
	if history := runLocal("log", "--workspace", linkedID); !strings.Contains(history, "linked snapshot") {
		t.Fatalf("prune lost linked worktree history: %s", history)
	}
	originalLog, err := os.ReadFile(filepath.Join(repo.Root, "original-hook.log"))
	if err != nil || !bytes.Contains(originalLog, []byte("original post-commit")) {
		t.Fatalf("existing post-commit hook did not run: %v %q", err, originalLog)
	}
	runLocal("auto", "uninstall")
	if status := runLocal("hook", "status"); !strings.Contains(status, "pre-commit: installed") || !strings.Contains(status, "pre-push: installed") {
		t.Fatalf("automatic hook uninstall affected guards: %s", status)
	}
	if got, err := os.ReadFile(filepath.Join(hooks, "post-commit")); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("uninstall did not restore original post-commit hook: %v %q", err, got)
	}
	for _, name := range []string{"post-checkout", "pre-rebase", "post-rewrite"} {
		if _, err := os.Stat(filepath.Join(hooks, name)); !os.IsNotExist(err) {
			t.Fatalf("uninstall left %s hook: %v", name, err)
		}
	}
	runLocal("hook", "uninstall")
}

func TestAutomaticWorktreeBootstrapCreatesFirstMainSnapshot(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	bin := testBinary(t)
	runLocal := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = cwd, repo.RunnerEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git-local %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	runLocal(repo.Root, "init")
	if err := os.WriteFile(filepath.Join(repo.Root, "first.local"), []byte("first unsaved version\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runLocal(repo.Root, "track", "first.local")
	runLocal(repo.Root, "auto", "install")
	if history := runLocal(repo.Root, "log"); !strings.Contains(history, "No local snapshots") {
		t.Fatalf("unexpected initial snapshot: %s", history)
	}
	linked := filepath.Join(t.TempDir(), "new-worktree")
	repo.Git("worktree", "add", "-q", "-b", "new-worktree", linked)
	if got, err := os.ReadFile(filepath.Join(linked, "first.local")); err != nil || !bytes.Equal(got, []byte("first unsaved version\n")) {
		t.Fatalf("bootstrap lost unsaved main content: %v %q", err, got)
	}
	if history := runLocal(repo.Root, "log"); !strings.Contains(history, "before worktree bootstrap") {
		t.Fatalf("bootstrap did not create first main snapshot: %s", history)
	}
	if status := runLocal(linked, "status"); !strings.Contains(status, "first.local") {
		t.Fatalf("new worktree did not inherit selection: %s", status)
	}
	noCheckout := filepath.Join(t.TempDir(), "no-checkout")
	repo.Git("worktree", "add", "-q", "--no-checkout", "-b", "no-checkout", noCheckout)
	noCheckoutAdmin := strings.TrimSpace(string(repo.At(noCheckout, "rev-parse", "--absolute-git-dir")))
	if _, err := os.Stat(filepath.Join(noCheckoutAdmin, "gitlocal-workspace.json")); !os.IsNotExist(err) {
		t.Fatalf("--no-checkout worktree was initialized before checkout: %v", err)
	}
	repo.At(noCheckout, "checkout", "-q", "-b", "no-checkout-ready")
	if got, err := os.ReadFile(filepath.Join(noCheckout, "first.local")); err != nil || !bytes.Equal(got, []byte("first unsaved version\n")) {
		t.Fatalf("later checkout did not seed --no-checkout worktree: %v %q", err, got)
	}
	if status := runLocal(noCheckout, "status"); !strings.Contains(status, "first.local") {
		t.Fatalf("recovered --no-checkout worktree has no selection: %s", status)
	}
}
