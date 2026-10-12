package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// SeedFilesBeside rewrites seed journal id (pending, or completed when
// completed is set) in the private state directory state into the form
// git-local writes for a journal too large to hold its file list: the list in
// transactions/seed-files/<id>.json, named by digest from the journal.
func SeedFilesBeside(t testing.TB, state, id string, completed bool) {
	t.Helper()
	name := filepath.Join(state, "transactions", "seed-"+id+".json")
	if completed {
		name = filepath.Join(state, "transactions", "completed", "seed-"+id+".json")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]any
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	list, err := json.MarshalIndent(map[string]any{"files": journal["files"]}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	list = append(list, '\n')
	dir := filepath.Join(state, "transactions", "seed-files")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), list, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(list)
	journal["filesDigest"] = hex.EncodeToString(sum[:])
	journal["files"] = []any{}
	if data, err = json.MarshalIndent(journal, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
