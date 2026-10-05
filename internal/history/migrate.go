package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type MigrationResult struct {
	Heads     map[string]string
	Snapshots map[string]string
}

// HistoricalPaths lists every repository-relative file path recorded by any
// snapshot reachable from a ref in the private store. Migration uses it so a
// sensitive rule matches what history holds, not only today's checkout.
func HistoricalPaths(ctx context.Context, r *gitexec.Runner, w workspace.Context) (map[string]bool, error) {
	paths := map[string]bool{}
	commits, err := r.History(ctx, w.Store, nil, "rev-list", "--all")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(commits)) == 0 {
		return paths, nil
	}
	b, err := r.History(ctx, w.Store, bytes.NewReader(commits), "diff-tree", "--stdin", "--root", "-r", "--no-commit-id", "--no-renames", "--name-only", "-z")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(string(b), "\x00") {
		if rest, ok := strings.CutPrefix(name, "files/"); ok && rest != "" {
			paths[rest] = true
		}
	}
	return paths, nil
}

// RewritePlaintextStore copies validated user histories into a new bare store,
// encrypting every file covered by policy. It refuses other refs because their
// objects may contain plaintext and cannot safely be carried over unchanged.
// The caller holds the common repository lock and owns the empty destination.
func RewritePlaintextStore(ctx context.Context, r *gitexec.Runner, w workspace.Context, destination string, policy secureblob.Policy, key []byte, allowedRecovery ...map[string]string) (MigrationResult, error) {
	if err := gitexec.ValidateStore(destination); err != nil {
		return MigrationResult{}, err
	}
	refs, err := r.History(ctx, w.Store, nil, "show-ref")
	if err != nil {
		var ge *gitexec.Error
		if !errors.As(err, &ge) || ge.Code != 1 {
			return MigrationResult{}, err
		}
	}
	heads := map[string]string{}
	recovery := map[string]string{}
	if len(allowedRecovery) > 0 {
		recovery = allowedRecovery[0]
	}
	seenRecovery := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validOID(fields[0]) {
			return MigrationResult{}, errors.New("malformed private history ref")
		}
		const prefix = "refs/gitlocal/workspaces/"
		if !strings.HasPrefix(fields[1], prefix) || !strings.HasSuffix(fields[1], "/head") {
			if recovery[fields[1]] == fields[0] && !seenRecovery[fields[1]] {
				seenRecovery[fields[1]] = true
				continue
			}
			return MigrationResult{}, fmt.Errorf("%w: private ref %q requires a migration handler", storage.ErrConflict, fields[1])
		}
		id := strings.TrimSuffix(strings.TrimPrefix(fields[1], prefix), "/head")
		if !storage.ValidUUID(id) || fields[1] != Ref(id) {
			return MigrationResult{}, errors.New("invalid workspace history ref")
		}
		if err := configWorkspace(w, id); err != nil {
			return MigrationResult{}, err
		}
		heads[id] = fields[0]
	}
	if len(seenRecovery) != len(recovery) {
		return MigrationResult{}, errors.New("completed restore journals and recovery refs differ")
	}
	ids := make([]string, 0, len(heads))
	for id := range heads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := MigrationResult{Heads: map[string]string{}, Snapshots: map[string]string{}}
	for _, source := range ids {
		if _, err := AuditWorkspaceLocked(ctx, r, w, source); err != nil {
			return MigrationResult{}, fmt.Errorf("audit workspace %s before migration: %w", source, err)
		}
		out, err := r.History(ctx, w.Store, nil, "rev-list", "--reverse", "--parents", heads[source])
		if err != nil {
			return MigrationResult{}, err
		}
		previousOld, previousNew := "", ""
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 1 || len(fields) > 2 || !validOID(fields[0]) || len(fields) == 2 && fields[1] != previousOld {
				return MigrationResult{}, errors.New("nonlinear workspace history during migration")
			}
			if previousOld == "" && len(fields) != 1 || previousOld != "" && len(fields) != 2 {
				return MigrationResult{}, errors.New("incomplete workspace history during migration")
			}
			next, err := rewriteSnapshot(ctx, r, w, destination, source, fields[0], previousNew, policy, key)
			if err != nil {
				return MigrationResult{}, fmt.Errorf("rewrite snapshot %s: %w", fields[0], err)
			}
			result.Snapshots[fields[0]] = next
			previousOld, previousNew = fields[0], next
		}
		if previousOld != heads[source] || previousNew == "" {
			return MigrationResult{}, errors.New("workspace history changed during migration")
		}
		if err := advance(ctx, r, destination, Ref(source), previousNew, ""); err != nil {
			return MigrationResult{}, err
		}
		result.Heads[source] = previousNew
	}
	return result, nil
}

// SnapshotFileFromStore reads one file from a known reachable snapshot ID.
// Migration callers must establish reachability before using it.
func SnapshotFileFromStore(ctx context.Context, r *gitexec.Runner, w workspace.Context, store, source, snapshotID, path string) (SnapshotFile, string, error) {
	tree, metadata, err := readPrevious(ctx, r, store, snapshotID, w.RepositoryID, source)
	if err != nil {
		return SnapshotFile{}, "", err
	}
	files, err := readFiles(ctx, r, store, tree)
	if err != nil {
		return SnapshotFile{}, "", err
	}
	file, ok := files[path]
	if !ok {
		return SnapshotFile{}, "", fmt.Errorf("snapshot has no file %q", path)
	}
	return SnapshotFile{Mode: file.Mode, ID: file.ID}, metadata.EncryptedDigests[path], nil
}

// VerifyEncryptedStore authenticates all migrated user snapshots before the
// policy is published. It does not read a policy from the common store.
func VerifyEncryptedStore(ctx context.Context, r *gitexec.Runner, w workspace.Context, store string, policy secureblob.Policy, key []byte, heads map[string]string, otherRefs ...map[string]string) error {
	if err := gitexec.ValidateStore(store); err != nil {
		return err
	}
	if _, err := r.History(ctx, store, nil, "fsck", "--full", "--no-reflogs"); err != nil {
		return err
	}
	other := map[string]string{}
	if len(otherRefs) > 0 {
		other = otherRefs[0]
	}
	refs, err := r.History(ctx, store, nil, "show-ref")
	if err != nil {
		var ge *gitexec.Error
		if !errors.As(err, &ge) || ge.Code != 1 || len(heads)+len(other) != 0 {
			return err
		}
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validOID(fields[0]) {
			return errors.New("migrated history refs differ from prepared heads")
		}
		if other[fields[1]] == fields[0] {
			if seen[fields[1]] {
				return errors.New("duplicate migrated recovery ref")
			}
			seen[fields[1]] = true
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(fields[1], "refs/gitlocal/workspaces/"), "/head")
		if fields[1] != Ref(id) || heads[id] != fields[0] || seen[fields[1]] {
			return errors.New("unexpected migrated history ref")
		}
		seen[fields[1]] = true
	}
	if len(seen) != len(heads)+len(other) {
		return errors.New("migrated history is missing workspace heads")
	}
	for source, tip := range heads {
		out, err := r.History(ctx, store, nil, "rev-list", "--parents", tip)
		if err != nil {
			return err
		}
		expected := tip
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 1 || len(fields) > 2 || fields[0] != expected || !validOID(fields[0]) {
				return errors.New("malformed migrated snapshot ancestry")
			}
			if len(fields) == 2 {
				expected = fields[1]
			} else {
				expected = ""
			}
			filesTree, metadata, err := readPrevious(ctx, r, store, fields[0], w.RepositoryID, source)
			if err != nil {
				return err
			}
			if metadata.SchemaVersion != 2 || metadata.EncryptedKeyFingerprint != policy.KeyFingerprint {
				return errors.New("migrated snapshot has wrong encryption schema or key")
			}
			files, err := readFiles(ctx, r, store, filesTree)
			if err != nil {
				return err
			}
			if len(metadata.EncryptedDigests) > len(files) {
				return errors.New("migrated snapshot has extra encrypted digests")
			}
			for path, file := range files {
				if !metadata.Selection.Selected(path) || policy.Covers(path) != (metadata.EncryptedDigests[path] != "") {
					return fmt.Errorf("migrated snapshot content disagrees at %q", path)
				}
				kind, err := r.History(ctx, store, nil, "cat-file", "-t", file.ID)
				if err != nil || string(kind) != "blob\n" {
					return fmt.Errorf("migrated file %q is not a blob: %w", path, err)
				}
				if policy.Covers(path) {
					identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: source, Path: path, Mode: file.Mode}
					digest, err := auditedBlobDigest(ctx, r, store, file.ID, key, identity)
					if err != nil {
						return fmt.Errorf("migrated file %q failed authentication: %w", path, err)
					}
					if digest != metadata.EncryptedDigests[path] {
						return fmt.Errorf("migrated file %q digest differs", path)
					}
				}
			}
		}
		if expected != "" {
			return errors.New("incomplete migrated snapshot ancestry")
		}
	}
	return nil
}

func configWorkspace(w workspace.Context, id string) error {
	d, err := storage.OpenDir(w.Common)
	if err != nil {
		return err
	}
	defer d.Close()
	var record workspace.Record
	if err := d.ReadJSON("gitlocal/workspaces/"+id+"/workspace.json", &record); err != nil {
		return err
	}
	if record.SchemaVersion != workspace.SchemaVersion || record.WorkspaceID != id || record.State != "active" && record.State != "retained" {
		return errors.New("invalid workspace history owner")
	}
	return nil
}

func rewriteSnapshot(ctx context.Context, r *gitexec.Runner, w workspace.Context, destination, source, oldID, parent string, policy secureblob.Policy, key []byte) (string, error) {
	treeID, metadata, err := readPrevious(ctx, r, w.Store, oldID, w.RepositoryID, source)
	if err != nil {
		return "", err
	}
	if metadata.SchemaVersion != 1 {
		return "", errors.New("mixed or already encrypted history requires a separate migration")
	}
	oldFiles, err := readFiles(ctx, r, w.Store, treeID)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, len(oldFiles))
	for path := range oldFiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	root := &directory{}
	digests := map[string]string{}
	for _, path := range paths {
		file := oldFiles[path]
		id := file.ID
		if rule, ok := policy.UncoveredAlias(path); ok {
			return "", fmt.Errorf("%w: historical file %q matches sensitive path %q only by spelling or kind and would stay unencrypted", storage.ErrConflict, path, rule)
		}
		if policy.Covers(path) {
			identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: source, Path: path, Mode: file.Mode}
			id, digests[path], err = migrateSensitiveBlob(ctx, r, w.Store, destination, file.ID, key, identity)
		} else {
			id, err = migratePublicBlob(ctx, r, w.Store, destination, file.ID)
		}
		if err != nil {
			return "", fmt.Errorf("file %q: %w", path, err)
		}
		root.add(path, file.Mode, id)
	}
	filesTree, err := root.write(ctx, r, destination)
	if err != nil {
		return "", err
	}
	metadata.SchemaVersion = 2
	metadata.EncryptedKeyFingerprint = policy.KeyFingerprint
	metadata.EncryptedDigests = digests
	data, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if len(data) > 1<<20 {
		return "", errors.New("migrated snapshot metadata exceeds size limit")
	}
	metadataID, err := hash(ctx, r, destination, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	wrapper, err := tree(ctx, r, destination, []treeEntry{{"040000", "tree", filesTree, "files"}, {"100644", "blob", metadataID, "metadata.json"}})
	if err != nil {
		return "", err
	}
	return rewriteCommit(ctx, r, w.Store, destination, oldID, wrapper, parent)
}

func migratePublicBlob(ctx context.Context, r *gitexec.Runner, source, destination, original string) (string, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := r.HistoryBlobTo(ctx, source, original, writer)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	id, hashErr := hash(ctx, r, destination, reader)
	_ = reader.Close()
	readErr := <-done
	if readErr != nil {
		return "", readErr
	}
	if hashErr != nil {
		return "", hashErr
	}
	if id != original {
		return "", errors.New("public blob changed during migration")
	}
	return id, nil
}

func migrateSensitiveBlob(ctx context.Context, r *gitexec.Runner, source, destination, original string, key []byte, identity secureblob.Identity) (string, string, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := r.HistoryBlobTo(ctx, source, original, writer)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	id, digest, encryptErr := writeEncryptedBlob(ctx, r, destination, reader, key, identity)
	_ = reader.Close()
	readErr := <-done
	if readErr != nil {
		return "", "", readErr
	}
	if encryptErr != nil {
		return "", "", encryptErr
	}
	actual, err := auditedBlobDigest(ctx, r, destination, id, key, identity)
	if err != nil {
		return "", "", err
	}
	if actual != digest {
		return "", "", errors.New("encrypted copy failed authentication")
	}
	return id, digest, nil
}

func rewriteCommit(ctx context.Context, r *gitexec.Runner, source, destination, original, treeID, parent string) (string, error) {
	raw, err := r.History(ctx, source, nil, "cat-file", "commit", original)
	if err != nil {
		return "", err
	}
	header, message, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok {
		return "", errors.New("malformed snapshot commit")
	}
	lines := strings.Split(string(header), "\n")
	if len(lines) < 3 || len(lines) > 4 || !strings.HasPrefix(lines[0], "tree ") || !validOID(strings.TrimPrefix(lines[0], "tree ")) {
		return "", errors.New("unsupported snapshot commit header")
	}
	index := 1
	if strings.HasPrefix(lines[index], "parent ") {
		if parent == "" || !validOID(strings.TrimPrefix(lines[index], "parent ")) {
			return "", errors.New("invalid snapshot parent")
		}
		index++
	} else if parent != "" {
		return "", errors.New("missing snapshot parent")
	}
	if len(lines) != index+2 || !strings.HasPrefix(lines[index], "author ") || !strings.HasPrefix(lines[index+1], "committer ") {
		return "", errors.New("unsupported snapshot commit metadata")
	}
	var next bytes.Buffer
	fmt.Fprintf(&next, "tree %s\n", treeID)
	if parent != "" {
		fmt.Fprintf(&next, "parent %s\n", parent)
	}
	for _, line := range lines[index:] {
		next.WriteString(line)
		next.WriteByte('\n')
	}
	next.WriteByte('\n')
	next.Write(message)
	return oid(r.History(ctx, destination, &next, "hash-object", "-t", "commit", "-w", "--stdin", "--no-filters"))
}
