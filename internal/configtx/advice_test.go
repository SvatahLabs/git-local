package configtx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// GL-124: every other command's refusal of a pending seed whose file list is
// lost says so and names abandon (from the seed's destination), not resume
// and rollback, which both refuse. Before, it said "run git local recover
// <id> --resume or --rollback".
func TestPendingRefusalNamesAbandonForLostSeedList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, w, linked, id := sidecarSeed(t, "seed-file-0")
	if err := os.Remove(seedListPath(w, id)); err != nil {
		t.Fatal(err)
	}
	main := filepath.Dir(w.Common)
	for _, cwd := range []string{linked, main} {
		_, err := Change(ctx, r, cwd, true, "other")
		if err == nil || !strings.Contains(err.Error(), "seed-files/"+id+".json") || !strings.Contains(err.Error(), "is missing") || !strings.Contains(err.Error(), "recover "+id+" --abandon from "+w.Root) || strings.Contains(err.Error(), "--resume or --rollback") {
			t.Fatalf("refusal from %s for a seed whose list is lost: %v", cwd, err)
		}
	}
	// The command it names works.
	if _, err := Abandon(ctx, r, linked, id); err != nil {
		t.Fatal(err)
	}
}

// GL-124: a pending refusal names the one direction left after a commit or
// once a rollback started; an ordinary stop still names both.
func TestPendingRefusalNamesTheDirectionLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, w, linked, id := sidecarSeed(t, "seed-file-0")
	if _, err := Change(ctx, r, linked, true, "other"); err == nil || !strings.Contains(err.Error(), "recover "+id+" --resume or --rollback") {
		t.Fatalf("refusal for a stopped seed: %v", err)
	}
	name := filepath.Join(w.Common, "gitlocal", "transactions", "seed-"+id+".json")
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]any
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	journal["direction"] = "rollback"
	if data, err = json.Marshal(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Change(ctx, r, linked, true, "other")
	if err == nil || !strings.Contains(err.Error(), "rollback has started") || !strings.Contains(err.Error(), "recover "+id+" --rollback from "+w.Root) || strings.Contains(err.Error(), "--resume") {
		t.Fatalf("refusal for a seed whose rollback started: %v", err)
	}
	if _, err := RecoverSeed(ctx, r, linked, id, true); err != nil {
		t.Fatal(err)
	}
}

// GL-124: the quarantine records what was wrong with a seed's file list, not
// the abandon the user just ran, and says "doesn't match" only for a list
// that doesn't. Before, the journal's reason was the whole SeedListError
// (telling the user to run the abandon), and a list that couldn't be read
// was recorded as not matching.
func TestAbandonedSeedListReasons(t *testing.T) {
	for _, c := range []struct{ kind, journal, list string }{
		{"missing", "seed journal whose file list is missing", ""},
		{"mismatch", "seed journal whose file list doesn't match its journal", "seed file list that doesn't match its journal"},
		{"unreadable", "seed journal whose file list can't be read (", "seed file list that can't be read ("},
	} {
		t.Run(c.kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r, w, linked, id := sidecarSeed(t, "seed-file-0")
			list := seedListPath(w, id)
			switch c.kind {
			case "missing":
				if err := os.Remove(list); err != nil {
					t.Fatal(err)
				}
			case "mismatch":
				if err := os.WriteFile(list, []byte("{\"files\": []}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("needs a file this user can't read")
				}
				if err := os.Chmod(list, 0); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Abandon(ctx, r, linked, id)
			if err != nil {
				t.Fatal(err)
			}
			var manifest Quarantine
			data, err := os.ReadFile(filepath.Join(result.Quarantine, "manifest.json"))
			if err == nil {
				err = json.Unmarshal(data, &manifest)
			}
			if err != nil {
				t.Fatal(err)
			}
			reasons := map[string]string{}
			for _, entry := range manifest.Entries {
				reasons[entry.Kind] = entry.Reason
			}
			if !strings.HasPrefix(reasons["journal"], c.journal) || strings.Contains(reasons["journal"], "--abandon") {
				t.Fatalf("journal's quarantine reason: %q", reasons["journal"])
			}
			if !strings.HasPrefix(reasons["seed-files"], c.list) || c.list == "" && reasons["seed-files"] != "" {
				t.Fatalf("file list's quarantine reason: %q", reasons["seed-files"])
			}
		})
	}
}
