package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/storage"
)

// GL-069: maintenance packs the private store and deletes unreachable
// objects, keeping every snapshot and recovery ref; history, show and restore
// still work, and doctor reports the store's size.
func TestMaintenancePrunesUnreachableObjectsAndKeepsRefs(t *testing.T) {
	repo := layoutRepo(t)
	store := filepath.Join(repo.Root, ".git", "gitlocal", "history.git")
	gitStore := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(string(repo.Git(append([]string{"--git-dir=" + store}, args...)...)))
	}
	note := filepath.Join(repo.Root, "notes.local")
	for _, content := range []string{"v1\n", "v2\n", "v3\n"} {
		if err := os.WriteFile(note, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if content == "v1\n" {
			successful(t, repo.Root, "track", "notes.local")
		}
		successful(t, repo.Root, "commit", "-m", "notes "+strings.TrimSpace(content))
	}
	// A restore leaves a recovery ref whose preimage (v4) no snapshot holds.
	if err := os.WriteFile(note, []byte("v4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	log := successful(t, repo.Root, "log", "--json")
	var entries struct {
		Data struct {
			Entries []struct {
				ID string `json:"id"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(log), &entries); err != nil || len(entries.Data.Entries) != 3 {
		t.Fatalf("log: %v %s", err, log)
	}
	first := entries.Data.Entries[2].ID
	successful(t, repo.Root, "restore", "--source", first, "--overwrite", "--", "notes.local")
	refsBefore := gitStore("for-each-ref", "--format=%(refname) %(objectname)")
	if !strings.Contains(refsBefore, "refs/gitlocal/recovery/") {
		t.Fatalf("restore left no recovery ref:\n%s", refsBefore)
	}
	// Leftovers of interrupted snapshots: objects nothing refers to.
	var unreachable []string
	for _, content := range []string{"orphan one\n", "orphan two\n"} {
		blob := filepath.Join(t.TempDir(), "orphan")
		if err := os.WriteFile(blob, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		unreachable = append(unreachable, gitStore("hash-object", "-w", blob))
	}
	out := successful(t, repo.Root, "maintenance", "--json")
	var result struct {
		Data struct {
			Before struct{ LooseObjects, PackedObjects int64 }        `json:"before"`
			After  struct{ LooseObjects, PackedObjects, Packs int64 } `json:"after"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Data.Before.LooseObjects == 0 || result.Data.After.LooseObjects != 0 || result.Data.After.Packs != 1 || result.Data.After.PackedObjects >= result.Data.Before.LooseObjects+result.Data.Before.PackedObjects {
		t.Fatalf("maintenance result: %v %s", err, out)
	}
	for _, id := range unreachable {
		check := exec.Command(repo.GitPath, "--git-dir="+store, "cat-file", "-e", id)
		check.Env = repo.RunnerEnv()
		if err := check.Run(); err == nil {
			t.Fatalf("unreachable object %s survived maintenance", id)
		}
	}
	if refsAfter := gitStore("for-each-ref", "--format=%(refname) %(objectname)"); refsAfter != refsBefore {
		t.Fatalf("maintenance changed refs:\n%s\n---\n%s", refsBefore, refsAfter)
	}
	gitStore("fsck", "--full", "--strict", "--no-dangling")
	if got := successful(t, repo.Root, "show", first+":notes.local"); got != "v1\n" {
		t.Fatalf("show after maintenance: %q", got)
	}
	if out := successful(t, repo.Root, "log"); strings.Count(out, "notes v") != 3 {
		t.Fatalf("log after maintenance:\n%s", out)
	}
	if err := os.Remove(note); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "restore", "--source", "latest", "--", "notes.local")
	if data, err := os.ReadFile(note); err != nil || string(data) != "v3\n" {
		t.Fatalf("restore after maintenance: %q %v", data, err)
	}
	successful(t, repo.Root, "commit", "-m", "after maintenance")
	doctorReportsNoErrors(t, repo.Root)
	if out := successful(t, repo.Root, "doctor"); !strings.Contains(out, "loose objects") {
		t.Fatalf("doctor doesn't report the store size:\n%s", out)
	}
	// A pending transaction may name objects no ref holds yet.
	id, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(repo.Root, ".git", "gitlocal", "transactions", "config-"+id+".json")
	if err := os.WriteFile(journal, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, diagnostic := callCLI(t, repo.Root, "maintenance"); code == 0 || !strings.Contains(diagnostic, id) {
		t.Fatalf("maintenance with a pending transaction: exit %d %s", code, diagnostic)
	}
}
