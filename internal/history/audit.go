package history

import (
	"context"
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

// AuditWorkspaceLocked validates every user snapshot in one linear workspace
// history, including older trees that latest-only inspection would not visit.
// The caller holds the existing shared repository lock.
func AuditWorkspaceLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, workspaceID string) (int, error) {
	source, err := sourceWorkspace(w, workspaceID)
	if err != nil {
		return 0, err
	}
	tip, err := head(ctx, r, w.Store, Ref(source))
	if err != nil {
		return 0, err
	}
	if tip == "" {
		return 0, nil
	}
	raw, err := r.History(ctx, w.Store, nil, "rev-list", "--parents", tip)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	expected := tip
	count := 0
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 1 || len(fields) > 2 || fields[0] != expected || !validOID(fields[0]) {
			return count, errors.New("nonlinear or malformed user history")
		}
		if len(fields) == 2 {
			if !validOID(fields[1]) {
				return count, errors.New("invalid user history parent")
			}
			expected = fields[1]
		} else {
			expected = ""
		}
		tree, meta, e := readPrevious(ctx, r, w.Store, fields[0], w.RepositoryID, source)
		if e != nil {
			return count, fmt.Errorf("snapshot %s: %w", fields[0], e)
		}
		validated, e := validateSnapshotLocked(ctx, r, w, Snapshot{ID: fields[0], WorkspaceID: source, FilesTree: tree, Metadata: meta})
		if e != nil {
			return count, fmt.Errorf("snapshot %s: %w", fields[0], e)
		}
		if e = verifyEncryptedSnapshotLocked(ctx, r, w, validated); e != nil {
			return count, fmt.Errorf("snapshot %s: %w", fields[0], e)
		}
		count++
	}
	if expected != "" {
		return count, errors.New("incomplete user history")
	}
	now, err := head(ctx, r, w.Store, Ref(source))
	if err != nil {
		return count, err
	}
	if now != tip {
		return count, fmt.Errorf("%w: history changed during diagnosis", storage.ErrConflict)
	}
	return count, nil
}

func verifyEncryptedSnapshotLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, snapshot Snapshot) error {
	if len(snapshot.Metadata.EncryptedDigests) == 0 {
		return nil
	}
	secure, err := loadSecure(w)
	if err != nil {
		return err
	}
	if secure == nil || secure.policy.KeyFingerprint != snapshot.Metadata.EncryptedKeyFingerprint {
		return fmt.Errorf("%w: missing matching sensitive-file policy", storage.ErrConflict)
	}
	source := w
	source.WorkspaceID = snapshot.WorkspaceID
	paths := make([]string, 0, len(snapshot.Metadata.EncryptedDigests))
	for path := range snapshot.Metadata.EncryptedDigests {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := verifyEncryptedFile(ctx, r, source, snapshot, path, secure); err != nil {
			return err
		}
	}
	return nil
}

// VerifyEncryptedFileLocked authenticates one encrypted snapshot blob against
// its keyed metadata digest without computing or persisting a plain hash.
// Seed planners use this before creating a recovery journal.
func VerifyEncryptedFileLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, snapshot Snapshot, path string) error {
	secure, err := loadSecure(w)
	if err != nil {
		return err
	}
	if secure == nil || secure.policy.KeyFingerprint != snapshot.Metadata.EncryptedKeyFingerprint {
		return fmt.Errorf("%w: missing matching sensitive-file policy", storage.ErrConflict)
	}
	source := w
	source.WorkspaceID = snapshot.WorkspaceID
	return verifyEncryptedFile(ctx, r, source, snapshot, path, secure)
}

func verifyEncryptedFile(ctx context.Context, r *gitexec.Runner, source workspace.Context, snapshot Snapshot, path string, secure *secureState) error {
	file, ok := snapshot.Files[path]
	expected := snapshot.Metadata.EncryptedDigests[path]
	if !ok || !secure.covers(path) || !validDigest(expected) {
		return fmt.Errorf("%w: encrypted snapshot metadata disagrees at %q", storage.ErrConflict, path)
	}
	identity := secure.identity(source, path, file.Mode)
	actual, err := auditedBlobDigest(ctx, r, source.Store, file.ID, secure.key, identity)
	if err != nil {
		return fmt.Errorf("encrypted file %q: %w", path, err)
	}
	if actual != expected {
		return fmt.Errorf("%w: encrypted file %q digest differs", storage.ErrConflict, path)
	}
	return nil
}

func auditedBlobDigest(ctx context.Context, r *gitexec.Runner, store, blob string, key []byte, identity secureblob.Identity) (string, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := secureblob.DecryptGitBlob(ctx, r, store, blob, writer, key, identity)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	digest, digestErr := secureblob.Digest(reader, key, identity)
	_ = reader.Close()
	decryptErr := <-done
	if decryptErr != nil {
		return "", decryptErr
	}
	return digest, digestErr
}
