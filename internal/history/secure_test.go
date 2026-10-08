package history

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/keymaterial"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
)

func TestEncryptedBlobStaysCiphertextInPrivateGitStore(t *testing.T) {
	_, g, w := fixture(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{7}, 32)
	id := (&secureState{key: key}).identity(*w, ".env.local", "100644")
	secret := []byte("SERVICE_TOKEN=private-value\n")
	objectID, digest, err := writeEncryptedBlob(ctx, g, w.Store, bytes.NewReader(secret), key, id)
	if err != nil {
		t.Fatal(err)
	}
	if digest != mustSecureDigest(t, secret, key, id) {
		t.Fatal("encrypted stream digest differs from plaintext digest")
	}
	raw := gitHistory(t, g, w, "cat-file", "blob", objectID)
	if bytes.Equal(raw, secret) || bytes.Contains(raw, []byte("private-value")) {
		t.Fatal("private Git object contains plaintext secret")
	}
	result, err := readEncryptedBytes(ctx, g, w.Store, objectID, key, id, 1<<20)
	if err != nil || !bytes.Equal(result, secret) {
		t.Fatalf("decrypt private Git object: %q %v", result, err)
	}
	id.Path = "other"
	if _, err := readEncryptedBytes(ctx, g, w.Store, objectID, key, id, 1<<20); err == nil {
		t.Fatal("encrypted Git object accepted for another path")
	}
	id.Path = ".env.local"
	if _, err := readEncryptedBytes(ctx, g, w.Store, objectID, key, id, 4); err == nil || !strings.Contains(err.Error(), "read limit") {
		t.Fatalf("encrypted Git object bypassed read limit: %v", err)
	}
}

func mustSecureDigest(t *testing.T, content, key []byte, id secureblob.Identity) string {
	t.Helper()
	digest, err := secureblob.Digest(bytes.NewReader(content), key, id)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestFullHistoryAuditRejectsTamperedEncryptedBlob(t *testing.T) {
	repo, g, w := fixture(t)
	ctx := context.Background()
	write(t, repo.Root, ".env.local", []byte("TOKEN=audit-secret\n"))
	change(t, g, repo.Root, true, ".env.local")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	first := save(t, g, repo.Root)
	filesTree, _, err := readPrevious(ctx, g, w.Store, first.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := gitHistory(t, g, w, "cat-file", "blob", first.ID+":files/.env.local")
	ciphertext[len(ciphertext)-1] ^= 1
	tamperedBlob, err := hash(ctx, g, w.Store, bytes.NewReader(ciphertext))
	if err != nil {
		t.Fatal(err)
	}
	tamperedFiles, err := tree(ctx, g, w.Store, []treeEntry{{"100644", "blob", tamperedBlob, ".env.local"}})
	if err != nil || tamperedFiles == filesTree {
		t.Fatalf("tampered files tree: %q %v", tamperedFiles, err)
	}
	metadataID, err := oid(g.History(ctx, w.Store, nil, "rev-parse", first.ID+":metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := tree(ctx, g, w.Store, []treeEntry{{"040000", "tree", tamperedFiles, "files"}, {"100644", "blob", metadataID, "metadata.json"}})
	if err != nil {
		t.Fatal(err)
	}
	tamperedCommit, err := oid(g.History(ctx, w.Store, strings.NewReader("tampered encrypted snapshot\n"), "commit-tree", wrapper))
	if err != nil {
		t.Fatal(err)
	}
	if err := advance(ctx, g, w.Store, Ref(w.WorkspaceID), tamperedCommit, first.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := AuditWorkspaceLocked(ctx, g, *w, w.WorkspaceID); err == nil || count != 0 {
		t.Fatalf("audit accepted tampered encrypted snapshot: %d %v", count, err)
	}
}

func TestSensitiveSnapshotEncryptsContentAndReusesUnchangedBlob(t *testing.T) {
	repo, g, w := fixture(t)
	ctx := context.Background()
	write(t, repo.Root, ".env.local", []byte("TOKEN=first-private-value\n"))
	change(t, g, repo.Root, true, ".env.local")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	common.Close()
	first := save(t, g, repo.Root)
	filesTree, metadata, err := readPrevious(ctx, g, w.Store, first.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil || metadata.SchemaVersion != 2 || metadata.EncryptedDigests[".env.local"] == "" {
		t.Fatalf("encrypted metadata: %+v %v", metadata, err)
	}
	files, err := readFiles(ctx, g, w.Store, filesTree)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Resolve(ctx, g, repo.Root, "", "latest")
	if err != nil {
		t.Fatal(err)
	}
	plainID, err := PlainBlobIDLocked(ctx, g, *w, snapshot, ".env.local")
	if err != nil {
		t.Fatal(err)
	}
	expectedPlainID, err := hashRead(ctx, g, w.Store, strings.NewReader("TOKEN=first-private-value\n"), false)
	if err != nil || plainID != expectedPlainID {
		t.Fatalf("plaintext object ID: %q want %q, %v", plainID, expectedPlainID, err)
	}
	raw := gitHistory(t, g, w, "cat-file", "blob", files[".env.local"].ID)
	if bytes.Contains(raw, []byte("first-private-value")) {
		t.Fatal("private history blob contains plaintext secret")
	}
	if shown, err := Show(ctx, g, repo.Root, "", "latest", ".env.local"); err != nil || string(shown) != "TOKEN=first-private-value\n" {
		t.Fatalf("encrypted historical show: %q %v", shown, err)
	}
	if found, err := Grep(ctx, g, repo.Root, "", "first-private-value", 5, 10); err != nil || len(found.Matches) != 1 || found.Matches[0].Path != ".env.local" {
		t.Fatalf("encrypted historical grep: %+v %v", found, err)
	}
	if status, err := Status(ctx, g, repo.Root); err != nil || len(status.Files) != 0 {
		t.Fatalf("unchanged encrypted status: %+v %v", status, err)
	}
	if diff, err := Diff(ctx, g, repo.Root); err != nil || diff.Patch != "" {
		t.Fatalf("unchanged encrypted diff: %+v %v", diff, err)
	}
	unchanged := save(t, g, repo.Root)
	if unchanged.Changed || unchanged.ID != first.ID {
		t.Fatalf("unchanged sensitive file created another snapshot: %+v", unchanged)
	}
	write(t, repo.Root, ".env.local", []byte("TOKEN=second-private-value\n"))
	if status, err := Status(ctx, g, repo.Root); err != nil || len(status.Files) != 1 || status.Files[0].Code != "M" {
		t.Fatalf("changed encrypted status: %+v %v", status, err)
	}
	if diff, err := Diff(ctx, g, repo.Root); err != nil || !strings.Contains(diff.Patch, "-TOKEN=first-private-value") || !strings.Contains(diff.Patch, "+TOKEN=second-private-value") {
		t.Fatalf("changed encrypted diff: %+v %v", diff, err)
	}
	second := save(t, g, repo.Root)
	if !second.Changed || second.ID == first.ID {
		t.Fatalf("changed sensitive file was not captured: %+v", second)
	}
	if patch, err := DiffSnapshots(ctx, g, repo.Root, "", first.ID, second.ID); err != nil || !strings.Contains(patch, "-TOKEN=first-private-value") || !strings.Contains(patch, "+TOKEN=second-private-value") {
		t.Fatalf("encrypted historical diff: %q %v", patch, err)
	}
	if entries, err := LogFiltered(ctx, g, repo.Root, "", 2, LogFilter{WithStat: true}); err != nil || len(entries) != 2 || entries[0].StatData == nil || entries[0].StatData.Files != 1 || entries[0].StatData.Binary != 0 || entries[0].StatData.Additions != 1 || entries[0].StatData.Deletions != 1 {
		t.Fatalf("encrypted historical stat: %+v %v", entries, err)
	}
	if count, err := AuditWorkspaceLocked(ctx, g, *w, w.WorkspaceID); err != nil || count != 2 {
		t.Fatalf("encrypted full-history audit: %d %v", count, err)
	}
	snapshot.Metadata.EncryptedDigests[".env.local"] = strings.Repeat("0", 64)
	if err := verifyEncryptedSnapshotLocked(ctx, g, *w, snapshot); err == nil {
		t.Fatal("audit accepted mismatched keyed ciphertext digest")
	}
	originalKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(ctx, g, repo.Root); err == nil {
		t.Fatal("wrong key permitted encrypted status")
	}
	if _, err := Show(ctx, g, repo.Root, "", "latest", ".env.local"); err == nil {
		t.Fatal("wrong key permitted encrypted show")
	}
	if err := os.WriteFile(keyPath, originalKey, 0600); err != nil {
		t.Fatal(err)
	}
	_, next, err := readPrevious(ctx, g, w.Store, second.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil || next.EncryptedDigests[".env.local"] == metadata.EncryptedDigests[".env.local"] {
		t.Fatalf("sensitive digest did not change: %+v %v", next, err)
	}
}

func TestSnapshotSkipsPathOnlyAliasingASensitiveRule(t *testing.T) {
	for _, tc := range []struct{ rule, file, other string }{
		{"config/secrets", "config/secrets/token", "config/plain.txt"},
		{"notes/secret.txt", "notes/Secret.txt", "notes/plain.txt"},
	} {
		for _, earlier := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/earlier=%v", tc.file, earlier), func(t *testing.T) {
				ctx := context.Background()
				repo, g, w := fixture(t)
				write(t, repo.Root, tc.file, []byte("TOKEN=alias-secret-v1\n"))
				write(t, repo.Root, tc.other, []byte("plain v1\n"))
				change(t, g, repo.Root, true, filepath.Dir(tc.other))
				if earlier {
					// An earlier release saved it as plaintext before the policy.
					if _, err := Commit(ctx, g, repo.Root, "before policy"); err != nil {
						t.Fatal(err)
					}
				}
				keyPath := filepath.Join(t.TempDir(), "sensitive.key")
				if err := keymaterial.Create(keyPath, repo.Root); err != nil {
					t.Fatal(err)
				}
				policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: tc.rule, Kind: "file"}})
				if err != nil {
					t.Fatal(err)
				}
				common, err := storage.OpenDir(w.Common)
				if err != nil {
					t.Fatal(err)
				}
				if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
					t.Fatal(err)
				}
				common.Close()
				write(t, repo.Root, tc.file, []byte("TOKEN=alias-secret-v2\n"))
				write(t, repo.Root, tc.other, []byte("plain v2\n"))
				result, err := Commit(ctx, g, repo.Root, "aliased")
				if err != nil {
					t.Fatalf("snapshot of %q under file rule %q: %v", tc.file, tc.rule, err)
				}
				if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], tc.file) || !strings.Contains(result.Warnings[0], "skipped") {
					t.Fatalf("warnings: %v", result.Warnings)
				}
				// The warning says what happened to the file: its last saved
				// version kept, or nothing saved at all.
				if kept := strings.Contains(result.Warnings[0], "its last saved version is kept"); kept != earlier || !earlier && !strings.Contains(result.Warnings[0], "has no saved version") {
					t.Fatalf("warning text (earlier=%v): %s", earlier, result.Warnings[0])
				}
				blob := func(content string) string {
					out, err := g.History(ctx, w.Store, strings.NewReader(content), "hash-object", "--stdin")
					if err != nil {
						t.Fatal(err)
					}
					return strings.TrimSpace(string(out))
				}
				if _, err := g.History(ctx, w.Store, nil, "cat-file", "-e", blob("TOKEN=alias-secret-v2\n")); err == nil {
					t.Fatal("the skipped file's new content was written as plaintext")
				}
				id, err := head(ctx, g, w.Store, Ref(w.WorkspaceID))
				if err != nil {
					t.Fatal(err)
				}
				tree, _, err := readPrevious(ctx, g, w.Store, id, w.RepositoryID, w.WorkspaceID)
				if err != nil {
					t.Fatal(err)
				}
				files, err := readFiles(ctx, g, w.Store, tree)
				if err != nil {
					t.Fatal(err)
				}
				if files[tc.other].ID != blob("plain v2\n") {
					t.Fatalf("other file not saved: %+v", files)
				}
				saved, ok := files[tc.file]
				if earlier && (!ok || saved.ID != blob("TOKEN=alias-secret-v1\n")) || !earlier && ok {
					t.Fatalf("skipped file not carried forward unchanged: %+v (earlier=%v)", files, earlier)
				}
			})
		}
	}
}

func TestSnapshotDropsSkippedPathAfterUntrackOrRename(t *testing.T) {
	for _, tc := range []struct {
		name, rule, file, other string
		change                  func(t *testing.T, g *gitexec.Runner, root string)
		saved                   string // path saved encrypted afterwards, if any
	}{
		{"untrack", "notes/secret.txt", "notes/Secret.txt", "notes/plain.txt", func(t *testing.T, g *gitexec.Runner, root string) {
			change(t, g, root, false, "notes/Secret.txt")
		}, ""},
		{"rename to the rule's spelling", "notes/secret.txt", "notes/Secret.txt", "notes/plain.txt", func(t *testing.T, g *gitexec.Runner, root string) {
			if err := os.Remove(filepath.Join(root, "notes/Secret.txt")); err != nil {
				t.Fatal(err)
			}
			write(t, root, "notes/secret.txt", []byte("TOKEN=alias-secret-v2\n"))
		}, "notes/secret.txt"},
		{"directory replaced by a file", "config/secrets", "config/secrets/token", "config/plain.txt", func(t *testing.T, g *gitexec.Runner, root string) {
			if err := os.RemoveAll(filepath.Join(root, "config/secrets")); err != nil {
				t.Fatal(err)
			}
			write(t, root, "config/secrets", []byte("TOKEN=alias-secret-v2\n"))
		}, "config/secrets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, g, w := fixture(t)
			parent := filepath.Dir(tc.other)
			write(t, repo.Root, tc.file, []byte("TOKEN=alias-secret-v1\n"))
			write(t, repo.Root, tc.other, []byte("plain v1\n"))
			change(t, g, repo.Root, true, parent)
			if _, err := Commit(ctx, g, repo.Root, "before policy"); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(t.TempDir(), "sensitive.key")
			if err := keymaterial.Create(keyPath, repo.Root); err != nil {
				t.Fatal(err)
			}
			policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: tc.rule, Kind: "file"}})
			if err != nil {
				t.Fatal(err)
			}
			common, err := storage.OpenDir(w.Common)
			if err != nil {
				t.Fatal(err)
			}
			if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
				t.Fatal(err)
			}
			common.Close()
			tc.change(t, g, repo.Root)
			write(t, repo.Root, tc.other, []byte("plain v2\n"))
			if _, err := CommitScoped(ctx, g, repo.Root, "scoped", "", []string{parent}); err != nil {
				t.Fatalf("scoped snapshot after %s: %v", tc.name, err)
			}
			write(t, repo.Root, tc.other, []byte("plain v3\n"))
			result, err := Commit(ctx, g, repo.Root, "full")
			if err != nil {
				t.Fatalf("full snapshot after %s: %v", tc.name, err)
			}
			if len(result.Warnings) != 0 {
				t.Fatalf("warnings: %v", result.Warnings)
			}
			write(t, repo.Root, tc.other, []byte("plain v4\n"))
			if _, err := CommitScoped(ctx, g, repo.Root, "scoped other", "", []string{tc.other}); err != nil {
				t.Fatalf("scoped snapshot of %q: %v", tc.other, err)
			}
			blob := func(content string) string {
				out, err := g.History(ctx, w.Store, strings.NewReader(content), "hash-object", "--stdin")
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(out))
			}
			if _, err := g.History(ctx, w.Store, nil, "cat-file", "-e", blob("TOKEN=alias-secret-v2\n")); err == nil {
				t.Fatal("new sensitive content was written as plaintext")
			}
			id, err := head(ctx, g, w.Store, Ref(w.WorkspaceID))
			if err != nil {
				t.Fatal(err)
			}
			tree, metadata, err := readPrevious(ctx, g, w.Store, id, w.RepositoryID, w.WorkspaceID)
			if err != nil {
				t.Fatal(err)
			}
			files, err := readFiles(ctx, g, w.Store, tree)
			if err != nil {
				t.Fatal(err)
			}
			if files[tc.other].ID != blob("plain v4\n") {
				t.Fatalf("other file not saved: %+v", files)
			}
			if _, ok := files[tc.file]; ok {
				t.Fatalf("old entry %q kept: %+v", tc.file, files)
			}
			if tc.saved != "" {
				if _, ok := files[tc.saved]; !ok || metadata.EncryptedDigests[tc.saved] == "" {
					t.Fatalf("%q not saved encrypted: %+v %v", tc.saved, files, metadata.EncryptedDigests)
				}
			}
		})
	}
}
