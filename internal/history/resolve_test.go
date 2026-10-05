package history

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/selection"
	"gitlocal/internal/workspace"
)

func TestResolveAndHistoricalTracking(t *testing.T) {
	r, g, w := fixture(t)
	ctx := context.Background()
	write(t, r.Root, "notes/file", []byte("saved"))
	write(t, r.Root, "explicit", []byte("deleted later"))
	change(t, g, r.Root, true, "notes", "explicit")
	first := save(t, g, r.Root)
	os.Remove(filepath.Join(r.Root, "explicit"))
	latest := save(t, g, r.Root)
	for _, selector := range []string{"latest", first.ID, first.ID[:12], strings.ToUpper(first.ID[:12])} {
		snapshot, err := Resolve(ctx, g, r.Root, "", selector)
		if err != nil {
			t.Fatal(selector, err)
		}
		if selector == "latest" && snapshot.ID != latest.ID {
			t.Fatal(snapshot)
		}
		if snapshot.WorkspaceID != w.WorkspaceID || snapshot.Files["notes/file"].Mode != "100644" {
			t.Fatal(snapshot)
		}
	}

	// An unrelated blob sharing the abbreviated commit ID must make it ambiguous.
	var collision string
	for i := 0; i < 1<<22; i++ {
		candidate := fmt.Sprintf("prefix-collision-%d", i)
		sum := sha1.Sum([]byte(fmt.Sprintf("blob %d%c%s", len(candidate), 0, candidate)))
		if fmt.Sprintf("%x", sum)[:4] == first.ID[:4] {
			collision = candidate
			break
		}
	}
	if collision == "" {
		t.Fatal("failed to construct deterministic prefix collision")
	}
	if _, err := hash(ctx, g, w.Store, strings.NewReader(collision)); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(ctx, g, r.Root, "", first.ID[:4]); err == nil {
		t.Fatal("accepted ambiguous snapshot prefix")
	}
	change(t, g, r.Root, false, "notes", "explicit")
	os.RemoveAll(filepath.Join(r.Root, "notes"))
	before, err := os.ReadFile(filepath.Join(w.Admin, "gitlocal-workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := TrackFrom(ctx, g, r.Root, "", "latest", "notes", "explicit")
	if err != nil || !result.Changed {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(filepath.Join(r.Root, "notes")); !os.IsNotExist(err) {
		t.Fatal("track restored content", err)
	}
	after, _ := os.ReadFile(filepath.Join(w.Admin, "gitlocal-workspace.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("track changed identity")
	}
	err = configtx.ReadSelection(ctx, g, r.Root, func(_ workspace.Context, s selection.State, _ []string, _ func() error) error {
		if !s.Selected("notes/future") || !s.Selected("explicit") {
			t.Fatal(s)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = TrackFrom(ctx, g, r.Root, "", "latest", "notes/never-recorded"); err == nil {
		t.Fatal("inferred missing descendant type")
	}
	for _, selector := range []string{"HEAD", "refs/gitlocal/x", first.ID + "^", first.ID + ":files", "abc", "--help"} {
		if _, err = Resolve(ctx, g, r.Root, "", selector); err == nil {
			t.Fatal("accepted revision expression", selector)
		}
	}
	blob := strings.TrimSpace(string(gitHistory(t, g, w, "rev-parse", first.ID+":files/notes/file")))
	if _, err = Resolve(ctx, g, r.Root, "", blob); err == nil {
		t.Fatal("accepted blob as snapshot")
	}
	treeID := strings.TrimSpace(string(gitHistory(t, g, w, "rev-parse", first.ID+"^{tree}")))
	orphan, err := oid(g.History(ctx, w.Store, strings.NewReader("unreachable"), "commit-tree", treeID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Resolve(ctx, g, r.Root, "", orphan); err == nil {
		t.Fatal("accepted unreachable commit")
	}
	write(t, r.Root, "explicit", []byte("main owned"))
	r.Git("add", "-f", "explicit")
	if _, err = TrackFrom(ctx, g, r.Root, "", "latest", "explicit"); err == nil {
		t.Fatal("historical tracking bypassed ownership")
	}
}
func TestRetainedSourceLogAndDestinationIdentity(t *testing.T) {
	r, g, main := fixture(t)
	r.Commit()
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	r.Git("worktree", "add", "-qb", "linked", linked)
	source, err := workspace.Init(ctx, g, linked)
	if err != nil {
		t.Fatal(err)
	}
	write(t, linked, "historical", []byte("source bytes"))
	change(t, g, linked, true, "historical")
	saved := save(t, g, linked)
	// Removal/pruning must leave source history available before and after retirement.
	if err = os.RemoveAll(linked); err != nil {
		t.Fatal(err)
	}
	r.Git("worktree", "prune", "--expire=now")
	if _, err = configtx.Retire(ctx, g, r.Root, source.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	entries, err := LogWorkspace(ctx, g, r.Root, source.WorkspaceID, 20)
	if err != nil || len(entries) != 1 || entries[0].ID != saved.ID {
		t.Fatal(entries, err)
	}
	if _, err = Resolve(ctx, g, r.Root, "", saved.ID); err == nil {
		t.Fatal("source workspace not required")
	}
	identity, err := os.ReadFile(filepath.Join(main.Admin, "gitlocal-workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = TrackFrom(ctx, g, r.Root, source.WorkspaceID, "latest", "historical"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(main.Admin, "gitlocal-workspace.json"))
	if !bytes.Equal(identity, after) {
		t.Fatal("source replaced destination identity")
	}
	if _, err = os.Stat(filepath.Join(r.Root, "historical")); !os.IsNotExist(err) {
		t.Fatal("historical track restored bytes")
	}
	own, err := head(ctx, g, main.Store, Ref(main.WorkspaceID))
	if err != nil || own != "" {
		t.Fatal("source replaced destination history", err)
	}
	r2, g2, _ := fixture(t)
	if _, err = Resolve(ctx, g2, r2.Root, "", saved.ID); err == nil {
		t.Fatal("accepted other store snapshot")
	}
}

func TestSnapshotResolverRejectsUnsupportedTreesAndSchema(t *testing.T) {
	r, g, w := fixture(t)
	ctx := context.Background()
	write(t, r.Root, "local", []byte("local"))
	change(t, g, r.Root, true, "local")
	first := save(t, g, r.Root)
	files, m, err := readPrevious(ctx, g, w.Store, first.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"schema", "gitlink", "missing-blob"} {
		metadata := m
		filesID := files
		if which == "schema" {
			metadata.SchemaVersion = 999
		}
		if which == "gitlink" {
			filesID, err = tree(ctx, g, w.Store, []treeEntry{{"160000", "commit", first.ID, "local"}})
			if err != nil {
				t.Fatal(err)
			}
		}
		if which == "missing-blob" {
			filesID, err = oid(g.History(ctx, w.Store, strings.NewReader("100644 blob "+strings.Repeat("1", 40)+"\tlocal\x00"), "mktree", "-z", "--missing"))
			if err != nil {
				t.Fatal(err)
			}
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		metaID, err := hash(ctx, g, w.Store, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		wrapper, err := tree(ctx, g, w.Store, []treeEntry{{"040000", "tree", filesID, "files"}, {"100644", "blob", metaID, "metadata.json"}})
		if err != nil {
			t.Fatal(err)
		}
		bad, err := oid(g.History(ctx, w.Store, strings.NewReader("invalid fixture"), "commit-tree", wrapper, "-p", first.ID))
		if err != nil {
			t.Fatal(err)
		}
		gitHistory(t, g, w, "update-ref", Ref(w.WorkspaceID), bad)
		if _, err = Resolve(ctx, g, r.Root, "", "latest"); err == nil {
			t.Fatal("accepted unsupported snapshot", which)
		}
		gitHistory(t, g, w, "update-ref", Ref(w.WorkspaceID), first.ID, bad)
	}
}
