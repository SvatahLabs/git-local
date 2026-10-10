package history

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/selection"
)

func wrapperNames(t *testing.T, ctx context.Context, g historyGit, store, id string) []string {
	t.Helper()
	out, err := g.History(ctx, store, nil, "ls-tree", "--name-only", "-z", id)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// A first snapshot whose changed-path list alone exceeds the 1 MiB metadata
// limit, then untracking that directory, both succeed; a small snapshot keeps
// the inline format earlier releases read.
func TestLargeFirstSnapshotAndUntrackExceedInlineMetadata(t *testing.T) {
	ctx := context.Background()
	repo, g, w := fixture(t)
	const files = 5000
	name := strings.Repeat("long-file-name-", 13)
	for i := 0; i < files; i++ {
		write(t, repo.Root, filepath.Join("bulk", fmt.Sprintf("dir-%02d", i%50), fmt.Sprintf("%s%05d.txt", name, i)), []byte("bulk\n"))
	}
	write(t, repo.Root, "notes.txt", []byte("notes v1\n"))
	change(t, g, repo.Root, true, "bulk", "notes.txt")
	first, err := Commit(ctx, g, repo.Root, "first")
	if err != nil {
		t.Fatalf("first snapshot of %d files: %v", files, err)
	}
	snapshot, err := Resolve(ctx, g, repo.Root, w.WorkspaceID, "latest")
	if err != nil || snapshot.ID != first.ID || len(snapshot.Metadata.ChangedPaths) != files+1 || len(snapshot.Files) != files+1 {
		t.Fatalf("first snapshot: %d changed, %d files, %v", len(snapshot.Metadata.ChangedPaths), len(snapshot.Files), err)
	}
	if names := wrapperNames(t, ctx, g, w.Store, first.ID); strings.Join(names, ",") != "changed-paths,files,metadata.json" {
		t.Fatalf("large snapshot wrapper: %v", names)
	}
	change(t, g, repo.Root, false, "bulk")
	write(t, repo.Root, "notes.txt", []byte("notes v2\n"))
	second, err := Commit(ctx, g, repo.Root, "after untracking bulk")
	if err != nil {
		t.Fatalf("snapshot after untracking %d files: %v", files, err)
	}
	snapshot, err = Resolve(ctx, g, repo.Root, w.WorkspaceID, "latest")
	if err != nil || len(snapshot.Files) != 1 || len(snapshot.Metadata.ChangedPaths) != files+1 {
		t.Fatalf("snapshot after untracking: %d files, %d changed, %v", len(snapshot.Files), len(snapshot.Metadata.ChangedPaths), err)
	}
	write(t, repo.Root, "notes.txt", []byte("notes v3\n"))
	third, err := Commit(ctx, g, repo.Root, "small")
	if err != nil {
		t.Fatal(err)
	}
	if names := wrapperNames(t, ctx, g, w.Store, third.ID); strings.Join(names, ",") != "files,metadata.json" {
		t.Fatalf("small snapshot wrapper: %v (second %s)", names, second.ID)
	}
	if count, err := AuditWorkspaceLocked(ctx, g, *w, w.WorkspaceID); err != nil || count != 3 {
		t.Fatalf("audit: %d %v", count, err)
	}
}

// Encrypted digests of 10k sensitive files exceed the inline limit on every
// snapshot; they round-trip through their own blob.
func TestTenThousandEncryptedDigestsRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, g, w := fixture(t)
	state, err := selection.Canonical(selection.State{SchemaVersion: selection.SchemaVersion, Include: []selection.Rule{{Path: "secrets", Kind: "directory"}}})
	if err != nil {
		t.Fatal(err)
	}
	m := Metadata{SchemaVersion: 2, Kind: "user", RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Selection: state, EncryptedKeyFingerprint: strings.Repeat("ab", 32), EncryptedDigests: map[string]string{}}
	for i := 0; i < 10000; i++ {
		m.EncryptedDigests[fmt.Sprintf("secrets/%s-%05d", strings.Repeat("s", 40), i)] = fmt.Sprintf("%064x", i)
	}
	empty, err := tree(ctx, g, w.Store, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := writeWrapper(ctx, g, w.Store, empty, m)
	if err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	commit, err := oid(g.History(ctx, w.Store, strings.NewReader("digests\n"), "commit-tree", wrapper))
	if err != nil {
		t.Fatal(err)
	}
	if names := wrapperNames(t, ctx, g, w.Store, commit); strings.Join(names, ",") != "encrypted-digests.json,files,metadata.json" {
		t.Fatalf("wrapper: %v", names)
	}
	_, read, err := readPrevious(ctx, g, w.Store, commit, w.RepositoryID, w.WorkspaceID)
	if err != nil || len(read.EncryptedDigests) != len(m.EncryptedDigests) {
		t.Fatalf("read back %d digests: %v", len(read.EncryptedDigests), err)
	}
	for path, digest := range m.EncryptedDigests {
		if read.EncryptedDigests[path] != digest {
			t.Fatalf("digest of %s differs", path)
		}
	}
}
