package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
)

func callCLI(t *testing.T, cwd string, args ...string) (int, string, string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := os.Chdir(old); e != nil {
			t.Error(e)
		}
	}()
	var out, diagnostic bytes.Buffer
	code := Execute(context.Background(), args, &out, &diagnostic)
	return code, out.String(), diagnostic.String()
}
func successful(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	code, out, err := callCLI(t, cwd, args...)
	if code != 0 {
		t.Fatalf("git local %v: exit %d: %s", args, code, err)
	}
	return out
}

func TestAutoWatchJSONStopsWithSnapshotSummary(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("timer snapshot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo.Root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, diagnostic bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"auto", "watch", "--interval", "1s", "--json"}, &out, &diagnostic)
	}()
	deadline := time.After(20 * time.Second)
	for {
		entries, err := history.Log(context.Background(), r, repo.Root, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("watch exited before snapshot: %d %s", code, diagnostic.String())
		case <-deadline:
			t.Fatal("watch did not publish its initial snapshot")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 || diagnostic.Len() != 0 {
			t.Fatalf("watch stop: %d %s", code, diagnostic.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not stop after cancellation")
	}
	var envelope struct {
		SchemaVersion int    `json:"schemaVersion"`
		OK            bool   `json:"ok"`
		Command       string `json:"command"`
		Data          struct {
			Snapshots      int    `json:"snapshots"`
			LatestSnapshot string `json:"latestSnapshot"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.SchemaVersion != 1 || !envelope.OK || envelope.Command != "git local auto watch" || envelope.Data.Snapshots != 1 || envelope.Data.LatestSnapshot == "" {
		t.Fatalf("invalid watch JSON: %v %s", err, out.String())
	}
}

func TestSeedNewWorktreeFromMainAndSubset(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	if err := os.Mkdir(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"a": "first", "b": "excluded"} {
		if err := os.WriteFile(filepath.Join(repo.Root, "notes", name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	successful(t, repo.Root, "track", "notes")
	successful(t, repo.Root, "untrack", "notes/b")
	successful(t, repo.Root, "commit", "-m", "source notes")
	full := filepath.Join(t.TempDir(), "full")
	repo.Git("worktree", "add", "-q", "-b", "seed-full", full)
	successful(t, full, "init", "--from", "main")
	data, err := os.ReadFile(filepath.Join(full, "notes", "a"))
	if err != nil || string(data) != "first" {
		t.Fatalf("full seed content: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(full, "notes", "b")); !os.IsNotExist(err) {
		t.Fatalf("excluded path copied: %v", err)
	}
	subset := filepath.Join(t.TempDir(), "subset")
	repo.Git("worktree", "add", "-q", "-b", "seed-subset", subset)
	successful(t, subset, "init", "--from", "main", "--paths", "notes/a")
	data, err = os.ReadFile(filepath.Join(subset, "notes", "a"))
	if err != nil || string(data) != "first" {
		t.Fatalf("subset seed content: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(subset, "notes", "b")); !os.IsNotExist(err) {
		t.Fatalf("unselected path copied: %v", err)
	}
	if out := successful(t, subset, "doctor"); !strings.Contains(out, "Doctor found no blocking issues") {
		t.Fatal(out)
	}
}

func TestLogFiltersSnapshotTimeBranchAndMainlineCommit(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	if err := os.WriteFile(filepath.Join(repo.Root, "local"), []byte("main"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "local")
	first := successful(t, repo.Root, "commit", "-m", "main local")
	head := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	repo.Git("checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(repo.Root, "local"), []byte("feature"), 0600); err != nil {
		t.Fatal(err)
	}
	second := successful(t, repo.Root, "commit", "-m", "feature local")
	if out := successful(t, repo.Root, "log", "--branch", "main"); !strings.Contains(out, "main local") || strings.Contains(out, "feature local") {
		t.Fatalf("main branch query: %s", out)
	}
	if out := successful(t, repo.Root, "log", "--branch", "feature", "--main-commit", head); !strings.Contains(out, "feature local") || strings.Contains(out, "main local") {
		t.Fatalf("combined context query: %s", out)
	}
	if out := successful(t, repo.Root, "log", "--main-commit", head); !strings.Contains(out, "main local") || !strings.Contains(out, "feature local") {
		t.Fatalf("mainline commit query: %s", out)
	}
	if out := successful(t, repo.Root, "log", "--branch", "unrelated"); out != "No matching local snapshots.\n" {
		t.Fatalf("empty branch query: %s", out)
	}
	if first == second {
		t.Fatal("fixture did not create two snapshots")
	}
}

func TestBrowseOldSnapshotsWithoutRestoring(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	if err := os.Mkdir(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo.Root, "notes", "a")
	if err := os.WriteFile(path, []byte("needle first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes")
	successful(t, repo.Root, "commit", "-m", "old notes")
	first := strings.Fields(successful(t, repo.Root, "log"))[0]
	if err := os.WriteFile(path, []byte("needle second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "commit", "-m", "new notes")
	second := strings.Fields(successful(t, repo.Root, "log"))[0]
	if err := os.WriteFile(path, []byte("unsaved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out := successful(t, repo.Root, "show", first+":notes/a"); out != "needle first\n" {
		t.Fatalf("historical show: %q", out)
	}
	if out := successful(t, repo.Root, "diff", first, second); !strings.Contains(out, "-needle first") || !strings.Contains(out, "+needle second") {
		t.Fatalf("historical diff: %s", out)
	}
	if out := successful(t, repo.Root, "log", "--stat"); !strings.Contains(out, "notes/a") || !strings.Contains(out, "1 file changed") {
		t.Fatalf("historical stats: %s", out)
	}
	if out := successful(t, repo.Root, "grep", "needle"); !strings.Contains(out, first) || !strings.Contains(out, second) {
		t.Fatalf("historical grep: %s", out)
	}
	for _, args := range [][]string{{"show", "HEAD:notes/a"}, {"show", first + ":../app.txt"}, {"diff", "HEAD", second}, {"grep", "needle", "--snapshots", "101"}} {
		if code, _, _ := callCLI(t, repo.Root, args...); code != 2 {
			t.Fatalf("invalid browse arguments %v returned %d", args, code)
		}
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "unsaved\n" {
		t.Fatalf("browsing changed worktree: %v %q", err, got)
	}
}

func TestBackupKeyGenerationAndDestinationRefusal(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	successful(t, repo.Root, "backup", "keygen", keyPath)
	if data, err := os.ReadFile(keyPath); err != nil || len(data) != 32 {
		t.Fatalf("generated backup key: %d bytes, %v", len(data), err)
	}
	if code, _, _ := callCLI(t, repo.Root, "backup", "keygen", filepath.Join(repo.Root, "inside.key")); code == 0 {
		t.Fatal("key inside checkout was accepted")
	}
	repo.Git("remote", "add", "origin", "git@origin.example:project/main.git")
	repo.Git("remote", "add", "backup", "ssh://git@origin.example/project/backup.git")
	for _, name := range []string{"origin", "backup"} {
		if code, _, _ := callCLI(t, repo.Root, "backup", "push", name, "--key-file", keyPath); code == 0 {
			t.Fatalf("unsafe backup remote %s was accepted", name)
		}
	}
	if code, _, _ := callCLI(t, repo.Root, "backup", "push", "backup"); code != 2 {
		t.Fatalf("missing key file returned %d", code)
	}
	if code, _, _ := callCLI(t, repo.Root, "backup", "restore", "backup", "refs/heads/main", "--key-file", keyPath); code != 2 {
		t.Fatalf("invalid restore ref returned %d", code)
	}
	if code, _, _ := callCLI(t, repo.Root, "backup", "restore", "backup"); code != 2 {
		t.Fatalf("incomplete restore request returned %d", code)
	}
}

func TestScopedCommitAndWriterProvenanceCLI(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	if err := os.Mkdir(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(repo.Root, "notes", name), []byte(name+"0"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	successful(t, repo.Root, "track", "notes")
	successful(t, repo.Root, "commit", "-m", "baseline")
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(repo.Root, "notes", name), []byte(name+"1"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, _ := callCLI(t, repo.Root, "commit", "-m", "bad", "notes/a"); code != 2 {
		t.Fatalf("commit path without -- returned %d", code)
	}
	if out := successful(t, filepath.Join(repo.Root, "notes"), "commit", "-m", "writer A", "--writer", "agent-a", "--", "a"); !strings.Contains(out, "Saved local snapshot") {
		t.Fatal(out)
	}
	log := successful(t, repo.Root, "log", "--writer", "agent-a", "--path", "notes/a", "--provenance")
	if !strings.Contains(log, "writer: agent-a") || !strings.Contains(log, `changed: "notes/a"`) || strings.Contains(log, `changed: "notes/b"`) {
		t.Fatal(log)
	}
	if out := successful(t, repo.Root, "status"); !strings.Contains(out, "notes/b") {
		t.Fatalf("other writer's pending change disappeared: %s", out)
	}
}

func TestJSONAgentInterfaceForCoreCommands(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	type envelope struct {
		SchemaVersion int                        `json:"schemaVersion"`
		Command       string                     `json:"command"`
		OK            bool                       `json:"ok"`
		Data          map[string]json.RawMessage `json:"data"`
		Error         *struct {
			Code, Message string
		} `json:"error"`
	}
	call := func(args ...string) envelope {
		t.Helper()
		code, out, diagnostic := callCLI(t, repo.Root, args...)
		if code != 0 || diagnostic != "" {
			t.Fatalf("JSON command %v: exit %d, stderr %q, stdout %q", args, code, diagnostic, out)
		}
		var result envelope
		if err := json.Unmarshal([]byte(out), &result); err != nil || result.SchemaVersion != 1 || !result.OK || result.Error != nil || result.Data == nil {
			t.Fatalf("invalid JSON command %v: %v %s", args, err, out)
		}
		if _, fallback := result.Data["text"]; fallback {
			t.Fatalf("JSON command %v fell back to human text: %s", args, out)
		}
		return result
	}
	if got := call("--json", "init"); got.Command != "git local init" || len(got.Data["workspaceId"]) == 0 {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("private\x00bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	call("track", "notes.local", "--json")
	snapshot := call("commit", "-m", "first", "--writer", "agent-a", "--json")
	var id string
	if err := json.Unmarshal(snapshot.Data["snapshotId"], &id); err != nil || len(id) != 40 {
		t.Fatalf("snapshot ID: %q %v", snapshot.Data["snapshotId"], err)
	}
	for _, args := range [][]string{{"status", "--json"}, {"diff", "--json"}, {"grep", "private", "--json"}, {"log", "--json"}, {"workspace", "list", "--json"}, {"doctor", "--json"}, {"recover", "--list", "--json"}, {"hook", "status", "--json"}, {"auto", "status", "--json"}, {"overlay", "status", "--json"}, {"guard", "staged", "--json"}} {
		call(args...)
	}
	stats := call("log", "--stat", "--json")
	var listed []struct {
		Stat struct {
			Files   int `json:"files"`
			Binary  int `json:"binary"`
			Entries []struct {
				Path string `json:"path"`
			} `json:"entries"`
		} `json:"stat"`
	}
	if err := json.Unmarshal(stats.Data["entries"], &listed); err != nil || len(listed) != 1 || listed[0].Stat.Files != 1 || listed[0].Stat.Binary != 1 || listed[0].Stat.Entries[0].Path != "notes.local" {
		t.Fatalf("structured log stat: %+v %v", listed, err)
	}
	for _, args := range [][]string{{"diff", "latest", "latest", "--json"}, {"workspace", "prune", "--dry-run", "--json"}, {"hook", "install", "--json"}, {"hook", "uninstall", "--json"}, {"auto", "install", "--json"}, {"auto", "uninstall", "--json"}} {
		call(args...)
	}
	shown := call("show", "latest:notes.local", "--json")
	var encoded string
	if err := json.Unmarshal(shown.Data["contentBase64"], &encoded); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != "private\x00bytes" {
		t.Fatalf("binary show JSON: %q %v", decoded, err)
	}
	plan := call("restore", "--source", "latest", "--dry-run", "--json", "--", "notes.local")
	if len(plan.Data["operations"]) == 0 {
		t.Fatal("restore plan omitted structured operations", plan)
	}
	call("restore", "--source", "latest", "--json", "--", "notes.local")
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	if got := call("backup", "keygen", keyPath, "--json"); len(got.Data["path"]) == 0 {
		t.Fatal(got)
	}
	repo.Git("remote", "add", "origin", "git@origin.example:project/main.git")
	repo.Git("remote", "add", "backup", "git@origin.example:project/archive.git")
	if pushGuard := call("guard", "push", "origin", "--json"); len(pushGuard.Data["findings"]) == 0 {
		t.Fatal("push guard omitted structured findings", pushGuard)
	}
	code, out, diagnostic := callCLI(t, repo.Root, "backup", "push", "backup", "--key-file", keyPath, "--json")
	if code != 1 || diagnostic != "" {
		t.Fatalf("JSON backup host refusal: exit %d stderr %q stdout %q", code, diagnostic, out)
	}
	var backupFailure envelope
	if err := json.Unmarshal([]byte(out), &backupFailure); err != nil || backupFailure.Error == nil || backupFailure.Error.Code != "operation" || len(backupFailure.Data["destinations"]) == 0 {
		t.Fatalf("invalid structured backup error: %v %s", err, out)
	}
	code, out, diagnostic = callCLI(t, repo.Root, "track", "--json")
	if code != 2 || diagnostic != "" {
		t.Fatalf("JSON usage error: exit %d stderr %q stdout %q", code, diagnostic, out)
	}
	var failed envelope
	if err := json.Unmarshal([]byte(out), &failed); err != nil || failed.OK || failed.Error == nil || failed.Error.Code != "usage" {
		t.Fatalf("invalid structured usage error: %v %s", err, out)
	}
	repo.Git("add", "-f", "notes.local")
	code, out, diagnostic = callCLI(t, repo.Root, "guard", "staged", "--json")
	if code != 1 || diagnostic != "" {
		t.Fatalf("JSON guard violation: exit %d stderr %q stdout %q", code, diagnostic, out)
	}
	failed = envelope{}
	if err := json.Unmarshal([]byte(out), &failed); err != nil || failed.Error == nil || failed.Error.Code != "guard_violation" || len(failed.Data["findings"]) <= 2 {
		t.Fatalf("invalid structured guard findings: %v %s", err, out)
	}
	repo.Git("reset", "-q", "HEAD", "--", "notes.local")
	if untracked := call("untrack", "notes.local", "--json"); len(untracked.Data["transactionId"]) == 0 {
		t.Fatal("untrack omitted structured transaction", untracked)
	}
}

func TestSeedPreservesExecutableAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires POSIX")
	}
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, repo.Root, "init")
	if err := os.Mkdir(filepath.Join(repo.Root, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "scripts", "run"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("scripts/run", filepath.Join(repo.Root, "run-link")); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "scripts/run", "run-link")
	successful(t, repo.Root, "commit", "-m", "source modes")
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "seed-modes", linked)
	successful(t, linked, "init", "--from", "main")
	info, err := os.Lstat(filepath.Join(linked, "scripts", "run"))
	if err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("executable lost: %v %v", info, err)
	}
	target, err := os.Readlink(filepath.Join(linked, "run-link"))
	if err != nil || target != "scripts/run" {
		t.Fatalf("symlink lost: %q %v", target, err)
	}
}
func TestCleanRestoreAndRetainedWorktreeExample(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	root := repo.Root
	home := filepath.Join(root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	successful(t, root, "init")
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes/a"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, root, "track", "notes/a")
	saved := successful(t, root, "commit", "-m", "original notes")
	source := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(saved, "Saved local snapshot ")), ".")
	if len(source) != 40 && len(source) != 64 {
		t.Fatal(saved)
	}
	if err := os.WriteFile(filepath.Join(root, "notes/a"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, root, "commit", "-m", "new notes")
	if out := successful(t, root, "doctor"); !strings.Contains(out, "Doctor found no blocking issues") {
		t.Fatal(out)
	}
	before, err := os.ReadFile(filepath.Join(root, "notes/a"))
	if err != nil {
		t.Fatal(err)
	}
	successful(t, root, "restore", "--source", source, "--dry-run", "--", "notes/a")
	after, err := os.ReadFile(filepath.Join(root, "notes/a"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("dry-run changed notes", err)
	}
	successful(t, root, "restore", "--source", source, "--", "notes/a")
	after, err = os.ReadFile(filepath.Join(root, "notes/a"))
	if err != nil || string(after) != "original" {
		t.Fatal(string(after), err)
	}
	if out := successful(t, root, "status"); !strings.Contains(out, "notes/a") {
		t.Fatal("restore did not remain visible", out)
	}
	if err := os.Remove(filepath.Join(root, "notes/a")); err != nil {
		t.Fatal(err)
	}
	successful(t, root, "restore", "--source", "latest", "--dry-run", "--", "notes/a")
	if _, err := os.Stat(filepath.Join(root, "notes/a")); !os.IsNotExist(err) {
		t.Fatal("dry-run recreated deleted file", err)
	}
	successful(t, root, "restore", "--source", "latest", "--", "notes/a")
	if restored, err := os.ReadFile(filepath.Join(root, "notes/a")); err != nil || string(restored) != "changed" {
		t.Fatal("deleted file was not restored", string(restored), err)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "aux", linked)
	initialized := successful(t, linked, "init")
	lines := strings.Split(initialized, "\n")
	fields := strings.Fields(lines[0])
	if len(fields) != 4 {
		t.Fatal(initialized)
	}
	sourceWorkspace := fields[3]
	if err = os.MkdirAll(filepath.Join(linked, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(linked, "notes/export"), []byte("from removed worktree"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, linked, "track", "notes/export")
	saved = successful(t, linked, "commit", "-m", "export")
	historical := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(saved, "Saved local snapshot ")), ".")
	repo.Git("worktree", "remove", "--force", linked)
	successful(t, root, "workspace", "retire", sourceWorkspace)
	if out := successful(t, root, "show", "--workspace", sourceWorkspace, historical+":notes/export"); out != "from removed worktree" {
		t.Fatalf("retained historical show: %q", out)
	}
	if out := successful(t, root, "grep", "--workspace", sourceWorkspace, "removed"); !strings.Contains(out, historical) {
		t.Fatalf("retained historical grep: %s", out)
	}
	if out := successful(t, root, "log", "--workspace", sourceWorkspace, "--stat"); !strings.Contains(out, "notes/export") {
		t.Fatalf("retained historical stat: %s", out)
	}
	successful(t, root, "track", "--from", historical, "--workspace", sourceWorkspace, "notes/export")
	planJSON := successful(t, root, "restore", "--source", historical, "--workspace", sourceWorkspace, "--dry-run", "--json", "--", "notes/export")
	var plan struct {
		Data struct {
			Operations []struct{ Path, Action string } `json:"operations"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(planJSON), &plan); err != nil || len(plan.Data.Operations) != 1 || plan.Data.Operations[0].Path != "notes/export" || plan.Data.Operations[0].Action != "create" {
		t.Fatal(planJSON, err)
	}
	successful(t, root, "restore", "--source", historical, "--workspace", sourceWorkspace, "--", "notes/export")
	data, err := os.ReadFile(filepath.Join(root, "notes/export"))
	if err != nil || string(data) != "from removed worktree" {
		t.Fatal(string(data), err)
	}
	if out := successful(t, root, "doctor"); !strings.Contains(out, "Doctor found no blocking issues") {
		t.Fatal(out)
	}
	id, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, ".git", "gitlocal", "transactions", "config-"+id+".json")
	if err := os.WriteFile(journal, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := callCLI(t, root, "doctor", "--json")
	if code != 3 {
		t.Fatal("doctor did not signal the pending transaction", code, out)
	}
	var report struct {
		SchemaVersion int `json:"schemaVersion"`
		Data          struct {
			Findings []struct {
				Check, Severity, Action string
			} `json:"findings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.SchemaVersion != 1 {
		t.Fatal("invalid doctor JSON", out, err)
	}
	found := false
	for _, finding := range report.Data.Findings {
		if finding.Check == "transaction "+id && finding.Severity == "error" && strings.Contains(finding.Action, id) {
			found = true
		}
	}
	if !found {
		t.Fatal("no actionable transaction finding", out)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("doctor changed the pending journal", err)
	}
}
