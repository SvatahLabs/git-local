package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestResetUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"reset"}, {"reset", "--hard", "HEAD", "extra"}} {
		var out, diagnostic bytes.Buffer
		if code := Execute(context.Background(), args, &out, &diagnostic); code != 2 {
			t.Fatalf("%v: exit %d, %s", args, code, diagnostic.String())
		}
	}
}

func TestResetHardCapturesSelectedFilesBeforeChangingMain(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	initial := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	successful(t, repo.Root, "init")
	notes := filepath.Join(repo.Root, "notes.local")
	if err := os.WriteFile(notes, []byte("secret before reset\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	app := filepath.Join(repo.Root, "app.txt")
	if err := os.WriteFile(app, []byte("second commit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.txt")
	repo.Git("commit", "-qm", "second")
	if err := os.WriteFile(notes, []byte("latest secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, []byte("dirty main edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	beforeInvalid := repo.Git("rev-parse", "HEAD")
	if code, _, _ := callCLI(t, repo.Root, "reset", "--hard", "not-a-revision"); code == 0 {
		t.Fatal("invalid revision was accepted")
	}
	if got := repo.Git("rev-parse", "HEAD"); string(got) != string(beforeInvalid) {
		t.Fatalf("invalid revision changed HEAD: %q != %q", got, beforeInvalid)
	}
	repo.Git("config", "filter.lfs.smudge", "sh -c 'exit 9'")
	out := successful(t, repo.Root, "reset", "--hard", "HEAD~1")
	if !strings.Contains(out, "Saved local snapshot") {
		t.Fatalf("reset did not report local capture: %s", out)
	}
	if got := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD"))); got != initial {
		t.Fatalf("main HEAD = %s, want %s", got, initial)
	}
	if got, err := os.ReadFile(app); err != nil || string(got) != "application\n" {
		t.Fatalf("main worktree not reset: %q %v", got, err)
	}
	if got, err := os.ReadFile(notes); err != nil || string(got) != "latest secret\n" {
		t.Fatalf("selected worktree file was changed: %q %v", got, err)
	}
	if got := successful(t, repo.Root, "show", "latest:notes.local"); got != "latest secret\n" {
		t.Fatalf("pre-reset version not saved: %q", got)
	}
	code, response, diagnostic := callCLI(t, repo.Root, "reset", "--hard", "--json")
	var envelope struct {
		SchemaVersion int  `json:"schemaVersion"`
		OK            bool `json:"ok"`
		Data          struct {
			Commit          string `json:"commit"`
			Reset           bool   `json:"reset"`
			SnapshotChanged bool   `json:"snapshotChanged"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); code != 0 || err != nil || diagnostic != "" || envelope.SchemaVersion != 1 || !envelope.OK || !envelope.Data.Reset || envelope.Data.Commit != initial || envelope.Data.SnapshotChanged {
		t.Fatalf("unchanged reset JSON: exit %d, %v, %s, %s", code, err, response, diagnostic)
	}
}

func TestResetHardRefusesUnsafeFilterAndSnapshotConflict(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	if err := os.WriteFile(filepath.Join(repo.Root, ".gitattributes"), []byte("app.txt filter=third-party\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", ".gitattributes")
	repo.Git("commit", "-qm", "configure checkout attribute")
	successful(t, repo.Root, "init")
	notes := filepath.Join(repo.Root, "notes.local")
	if err := os.WriteFile(notes, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	head := repo.Git("rev-parse", "HEAD")
	repo.Git("config", "filter.third-party.smudge", "sh -c 'exit 9'")
	if code, _, diagnostic := callCLI(t, repo.Root, "reset", "--hard"); code != 3 || !strings.Contains(diagnostic, "filter.third-party.smudge") {
		t.Fatalf("configured filter not rejected: %d %s", code, diagnostic)
	}
	repo.Git("config", "--unset", "filter.third-party.smudge")
	repo.Git("add", "-f", "notes.local")
	if code, _, diagnostic := callCLI(t, repo.Root, "reset", "--hard"); code != 3 || !strings.Contains(diagnostic, "snapshot selected files") {
		t.Fatalf("leaked selection did not stop reset: %d %s", code, diagnostic)
	}
	if got := repo.Git("rev-parse", "HEAD"); string(got) != string(head) {
		t.Fatalf("failed reset changed HEAD: %q != %q", got, head)
	}
}

func TestResetHardRejectsTargetTreeContainingSelectedPath(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	successful(t, repo.Root, "init")
	notes := filepath.Join(repo.Root, "notes.local")
	if err := os.WriteFile(notes, []byte("leaked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes.local")
	repo.Git("add", "-f", "notes.local")
	repo.Git("commit", "-qm", "leak")
	leaked := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	repo.Git("rm", "--cached", "notes.local")
	repo.Git("commit", "-qm", "remove leak")
	head := repo.Git("rev-parse", "HEAD")
	if code, _, diagnostic := callCLI(t, repo.Root, "reset", "--hard", leaked); code != 3 || !strings.Contains(diagnostic, "notes.local") {
		t.Fatalf("leaking reset target was accepted: %d %s", code, diagnostic)
	}
	if got := repo.Git("rev-parse", "HEAD"); string(got) != string(head) {
		t.Fatalf("failed reset changed HEAD: %q != %q", got, head)
	}
}

func TestResetHardRejectsTargetTreeOwningSelectedAncestor(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	successful(t, repo.Root, "init")
	notesDir := filepath.Join(repo.Root, "notes")
	if err := os.Mkdir(notesDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notesDir, "private"), []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "track", "notes/private")
	if err := os.RemoveAll(notesDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notesDir, []byte("tracked ancestor\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "notes")
	repo.Git("commit", "-qm", "ancestor")
	ancestor := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	repo.Git("rm", "notes")
	repo.Git("commit", "-qm", "remove ancestor")
	head := repo.Git("rev-parse", "HEAD")
	if err := os.Mkdir(notesDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notesDir, "private"), []byte("restored secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, diagnostic := callCLI(t, repo.Root, "reset", "--hard", ancestor); code != 3 || !strings.Contains(diagnostic, "notes") {
		t.Fatalf("selected ancestor was accepted: %d %s", code, diagnostic)
	}
	if got := repo.Git("rev-parse", "HEAD"); string(got) != string(head) {
		t.Fatalf("failed reset changed HEAD: %q != %q", got, head)
	}
}
