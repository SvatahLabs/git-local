package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type secureState struct {
	policy secureblob.Policy
	key    []byte
}

// loadSecure is called while the common repository lock is held. A published
// policy always requires its key, including when the current snapshot is empty.
func loadSecure(w workspace.Context) (*secureState, error) {
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	policy, exists, err := secureblob.ReadLocked(common, w.Root, w.Common)
	if err != nil || !exists {
		return nil, err
	}
	key, err := secureblob.LoadKey(policy, w.Root, w.Common)
	if err != nil {
		return nil, err
	}
	return &secureState{policy: policy, key: key}, nil
}

func (s *secureState) covers(path string) bool {
	return s != nil && s.policy.Covers(path)
}

// uncoveredAlias fails closed for a path that falls under a sensitive rule by
// alias key (case, normalization, or a file rule naming a directory) without
// being covered by its exact spelling, so it would otherwise be stored as
// plaintext.
func (s *secureState) uncoveredAlias(path string) error {
	if s == nil {
		return nil
	}
	if rule, ok := s.policy.UncoveredAlias(path); ok {
		return fmt.Errorf("%w: %q matches sensitive path %q only by spelling or kind and would be saved unencrypted; run git local doctor", storage.ErrConflict, path, rule)
	}
	return nil
}

func (s *secureState) identity(w workspace.Context, path, mode string) secureblob.Identity {
	return secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Path: path, Mode: mode}
}

func writeEncryptedBlob(ctx context.Context, g historyGit, store string, input io.Reader, key []byte, id secureblob.Identity) (string, string, error) {
	reader, writer := io.Pipe()
	type encryptionResult struct {
		digest string
		err    error
	}
	done := make(chan encryptionResult, 1)
	go func() {
		digest, err := secureblob.EncryptDigest(writer, input, key, id)
		_ = writer.CloseWithError(err)
		done <- encryptionResult{digest, err}
	}()
	objectID, hashErr := hash(ctx, g, store, reader)
	_ = reader.Close()
	result := <-done
	if result.err != nil {
		return "", "", result.err
	}
	return objectID, result.digest, hashErr
}

func readEncryptedBlob(ctx context.Context, r *gitexec.Runner, store, objectID string, dst io.Writer, key []byte, id secureblob.Identity) error {
	return secureblob.DecryptGitBlob(ctx, r, store, objectID, dst, key, id)
}

// decryptIntoTemporary hashes authenticated plaintext into a disposable Git
// object store for patch rendering. Callers must discard it if authentication
// fails; no plaintext is written into the durable private history store.
func decryptIntoTemporary(ctx context.Context, r *gitexec.Runner, sourceStore string, temporary historyGit, temporaryStore, objectID string, key []byte, id secureblob.Identity) (string, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := readEncryptedBlob(ctx, r, sourceStore, objectID, writer, key, id)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	plainID, hashErr := hashRead(ctx, temporary, temporaryStore, reader, true)
	_ = reader.Close()
	if decryptErr := <-done; decryptErr != nil {
		return "", decryptErr
	}
	return plainID, hashErr
}

// PlainBlobIDLocked computes a plaintext Git blob ID without writing plaintext
// into the durable history store. The snapshot must already be resolved under
// the common lock. Seed and restore planners use the ID for staged-file checks.
func PlainBlobIDLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, snapshot Snapshot, path string) (string, error) {
	file, ok := snapshot.Files[path]
	if !ok {
		return "", fmt.Errorf("path %q is not in the source snapshot", path)
	}
	if snapshot.Metadata.EncryptedDigests[path] == "" {
		return file.ID, nil
	}
	secure, err := loadSecure(w)
	if err != nil {
		return "", err
	}
	if secure == nil || secure.policy.KeyFingerprint != snapshot.Metadata.EncryptedKeyFingerprint || !secure.covers(path) {
		return "", fmt.Errorf("%w: missing matching sensitive-file policy for %q", storage.ErrConflict, path)
	}
	source := w
	source.WorkspaceID = snapshot.WorkspaceID
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := readEncryptedBlob(ctx, r, w.Store, file.ID, writer, secure.key, secure.identity(source, path, file.Mode))
		_ = writer.CloseWithError(err)
		done <- err
	}()
	plainID, hashErr := hashRead(ctx, r, w.Store, reader, false)
	_ = reader.Close()
	if decryptErr := <-done; decryptErr != nil {
		return "", decryptErr
	}
	return plainID, hashErr
}

func plainSnapshotTree(ctx context.Context, r *gitexec.Runner, w workspace.Context, snapshot Snapshot, temporary *gitexec.TemporaryHistory) (string, error) {
	files := make(map[string]fileValue, len(snapshot.Files))
	secure, err := loadSecure(w)
	if err != nil {
		return "", err
	}
	if snapshot.Metadata.SchemaVersion == 2 && (secure == nil || secure.policy.KeyFingerprint != snapshot.Metadata.EncryptedKeyFingerprint) {
		return "", fmt.Errorf("%w: missing matching sensitive-file key", storage.ErrConflict)
	}
	source := w
	source.WorkspaceID = snapshot.WorkspaceID
	for path, file := range snapshot.Files {
		if secure.covers(path) != (snapshot.Metadata.EncryptedDigests[path] != "") {
			return "", fmt.Errorf("%w: sensitive-file policy and snapshot content disagree for %q", storage.ErrConflict, path)
		}
		plainID := file.ID
		if snapshot.Metadata.EncryptedDigests[path] != "" {
			if !secure.covers(path) {
				return "", fmt.Errorf("%w: sensitive-file policy does not cover %q", storage.ErrConflict, path)
			}
			var err error
			plainID, err = decryptIntoTemporary(ctx, r, w.Store, temporary, temporary.Path, file.ID, secure.key, secure.identity(source, path, file.Mode))
			if err != nil {
				return "", err
			}
		}
		files[path] = fileValue{Mode: file.Mode, ID: plainID}
	}
	return treeFromFiles(ctx, temporary, temporary.Path, files, nil)
}

func snapshotStatPlain(ctx context.Context, r *gitexec.Runner, w workspace.Context, id, parent string) (string, StatSummary, error) {
	filesTree, metadata, err := readPrevious(ctx, r, w.Store, id, w.RepositoryID, w.WorkspaceID)
	if err != nil {
		return "", StatSummary{}, err
	}
	current, err := validateSnapshotLocked(ctx, r, w, Snapshot{ID: id, WorkspaceID: w.WorkspaceID, FilesTree: filesTree, Metadata: metadata})
	if err != nil {
		return "", StatSummary{}, err
	}
	temporary, err := r.TemporaryHistory(ctx, w.Store)
	if err != nil {
		return "", StatSummary{}, err
	}
	defer temporary.Close()
	beforeTree, err := treeFromFiles(ctx, temporary, temporary.Path, map[string]fileValue{}, nil)
	if err != nil {
		return "", StatSummary{}, err
	}
	if parent != "" {
		parentTree, parentMetadata, err := readPrevious(ctx, r, w.Store, parent, w.RepositoryID, w.WorkspaceID)
		if err != nil {
			return "", StatSummary{}, err
		}
		previous, err := validateSnapshotLocked(ctx, r, w, Snapshot{ID: parent, WorkspaceID: w.WorkspaceID, FilesTree: parentTree, Metadata: parentMetadata})
		if err != nil {
			return "", StatSummary{}, err
		}
		beforeTree, err = plainSnapshotTree(ctx, r, w, previous, temporary)
		if err != nil {
			return "", StatSummary{}, err
		}
	}
	afterTree, err := plainSnapshotTree(ctx, r, w, current, temporary)
	if err != nil {
		return "", StatSummary{}, err
	}
	out, err := temporary.History(ctx, temporary.Path, nil, "diff-tree", "--no-commit-id", "-r", "--numstat", "-z", "--no-ext-diff", "--no-textconv", "--no-renames", beforeTree, afterTree, "--")
	if err != nil {
		return "", StatSummary{}, err
	}
	return parseSnapshotStat(out, "")
}

type boundedWriter struct {
	dst   io.Writer
	limit int64
	count int64
}

var errReadLimit = errors.New("historical blob read limit exceeded")

func (w *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit-w.count {
		return 0, fmt.Errorf("%w: historical blob exceeds %d-byte read limit", errReadLimit, w.limit)
	}
	n, err := w.dst.Write(data)
	w.count += int64(n)
	if err == nil && n != len(data) {
		return n, io.ErrShortWrite
	}
	return n, err
}

func readEncryptedBytes(ctx context.Context, r *gitexec.Runner, store, objectID string, key []byte, id secureblob.Identity, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, errors.New("negative encrypted blob read limit")
	}
	var out bytes.Buffer
	writer := &boundedWriter{dst: &out, limit: limit}
	if err := readEncryptedBlob(ctx, r, store, objectID, writer, key, id); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
