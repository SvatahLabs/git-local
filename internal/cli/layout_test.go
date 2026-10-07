package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// layoutRepo is an initialized main checkout with an isolated HOME.
func layoutRepo(t *testing.T) *testutil.Repo {
	t.Helper()
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
	return repo
}

type listedWorkspace struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Root       string `json:"root"`
	Registered bool   `json:"registered"`
	Available  bool   `json:"available"`
}

func listWorkspaces(t *testing.T, cwd string) []listedWorkspace {
	t.Helper()
	out := successful(t, cwd, "workspace", "list", "--json")
	var envelope struct {
		Data struct {
			Workspaces []listedWorkspace `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("workspace list JSON: %v\n%s", err, out)
	}
	return envelope.Data.Workspaces
}

func doctorReportsNoErrors(t *testing.T, cwd string) {
	t.Helper()
	code, out, diagnostic := callCLI(t, cwd, "doctor", "--json")
	var envelope struct {
		Data struct {
			Findings []struct {
				Severity string `json:"severity"`
				Check    string `json:"check"`
				Message  string `json:"message"`
			} `json:"findings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("doctor JSON (exit %d): %v\n%s%s", code, err, out, diagnostic)
	}
	for _, finding := range envelope.Data.Findings {
		if finding.Severity == "error" {
			t.Fatalf("doctor error: %+v\n%s", finding, out)
		}
	}
}

// TestRelativeWorktreePathsWork: Git 2.48+ can record a linked worktree's
// gitdir pointer as a relative path. Lifecycle commands and doctor must
// resolve it.
func TestRelativeWorktreePathsWork(t *testing.T) {
	repo := layoutRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "--relative-paths", "-b", "relative", linked)
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(repo.Root, ".git", "worktrees"))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	pointer, err := os.ReadFile(filepath.Join(repo.Root, ".git", "worktrees", entries[0].Name(), "gitdir"))
	if err != nil || filepath.IsAbs(strings.TrimSpace(string(pointer))) {
		t.Fatalf("Git didn't record a relative gitdir pointer: %q %v", pointer, err)
	}
	if got := listWorkspaces(t, repo.Root); len(got) != 1 {
		t.Fatalf("workspaces before linked init: %+v", got)
	}
	successful(t, linked, "init")
	listed := listWorkspaces(t, repo.Root)
	if len(listed) != 2 {
		t.Fatalf("workspaces after linked init: %+v", listed)
	}
	for _, w := range listed {
		if !w.Registered || !w.Available {
			t.Fatalf("workspace not registered and available: %+v", w)
		}
	}
	doctorReportsNoErrors(t, repo.Root)
	doctorReportsNoErrors(t, linked)
	successful(t, repo.Root, "workspace", "prune")
	successful(t, repo.Root, "workspace", "remove", linked)
	for _, w := range listWorkspaces(t, repo.Root) {
		if w.Root == linked && w.State != "retained" {
			t.Fatalf("removed workspace not retained: %+v", w)
		}
	}
}
