package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type RecoveryMigrationIdentity struct {
	Ref         string
	OldCommit   string
	Destination string
	ID          string
}

type RewrittenRecovery struct {
	RecoveryMigrationIdentity
	NewCommit string
	Journal   []byte
}

func decodeMigrationJournal(data []byte) (journal, error) {
	var j journal
	if len(data) > journalLimit {
		return j, errors.New("restore journal exceeds size limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&j); err != nil {
		return j, err
	}
	if d.Decode(new(any)) != io.EOF {
		return j, errors.New("trailing restore journal data")
	}
	return j, nil
}

// InspectCompletedRecoveryForMigration verifies the original anchor and
// structural journal evidence before its ref is allowed through migration.
func InspectCompletedRecoveryForMigration(ctx context.Context, r *gitexec.Runner, w workspace.Context, data []byte) (RecoveryMigrationIdentity, error) {
	j, err := decodeMigrationJournal(data)
	if err != nil {
		return RecoveryMigrationIdentity{}, err
	}
	if j.SchemaVersion != 1 || j.Outcome == "" || !storage.ValidUUID(j.Manifest.Plan.DestinationWorkspaceID) {
		return RecoveryMigrationIdentity{}, errors.New("unsupported completed restore journal for encryption migration")
	}
	w.WorkspaceID = j.Manifest.Plan.DestinationWorkspaceID
	if err := validateJournalWithLiveState(ctx, r, w, j.Manifest.Plan.Selection, j.Manifest.ConfigDigest, j.ID, j, false); err != nil {
		return RecoveryMigrationIdentity{}, err
	}
	return RecoveryMigrationIdentity{Ref: RecoveryRef(w.WorkspaceID, j.ID), OldCommit: j.RecoverySnapshot, Destination: w.WorkspaceID, ID: j.ID}, nil
}

// RewriteCompletedRecovery copies a completed recovery anchor into the new
// store. Sensitive preimages are encrypted; no plaintext recovery object is
// copied into that store. The returned journal is staged until publication.
func RewriteCompletedRecovery(ctx context.Context, r *gitexec.Runner, w workspace.Context, destination string, data []byte, policy secureblob.Policy, key []byte, mapSnapshot func(string) (string, error)) (RewrittenRecovery, error) {
	identity, err := InspectCompletedRecoveryForMigration(ctx, r, w, data)
	if err != nil {
		return RewrittenRecovery{}, err
	}
	j, err := decodeMigrationJournal(data)
	if err != nil {
		return RewrittenRecovery{}, err
	}
	m := &j.Manifest
	p := &m.Plan
	oldSource := p.SourceSnapshot
	if p.SourceSnapshot, err = mapSnapshot(p.SourceSnapshot); err != nil {
		return RewrittenRecovery{}, err
	}
	if p.SourceHead, err = mapSnapshot(p.SourceHead); err != nil {
		return RewrittenRecovery{}, err
	}
	if p.DestinationBaseline != "" {
		if p.DestinationBaseline, err = mapSnapshot(p.DestinationBaseline); err != nil {
			return RewrittenRecovery{}, err
		}
	}
	protected := 0
	m.BeforeStored = make([]string, len(p.Operations))
	m.PostStored = make([]string, len(p.Operations))
	for i := range p.Operations {
		op := &p.Operations[i]
		isSensitive := policy.Covers(op.Path)
		op.Encrypted = isSensitive
		if isSensitive {
			protected++
		}
		if op.Before.Exists {
			actual, err := blobFingerprint(ctx, r, w.Store, op.Before.BlobID, op.Before.GitMode, op.Before.Permissions)
			if err != nil {
				return RewrittenRecovery{}, err
			}
			if actual != op.Before {
				return RewrittenRecovery{}, fmt.Errorf("restore preimage %q differs from anchor", op.Path)
			}
			if isSensitive {
				identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: p.DestinationWorkspaceID, Path: op.Path, Mode: op.Before.GitMode}
				stored, err := encryptRecoveryBlob(ctx, r, w.Store, destination, op.Before.BlobID, key, identity)
				if err != nil {
					return RewrittenRecovery{}, err
				}
				m.BeforeStored[i] = stored
				op.Before, err = protectedBlobFingerprint(ctx, r, destination, stored, key, identity, op.Before.Permissions)
				if err != nil {
					return RewrittenRecovery{}, err
				}
			} else {
				if err := copyRecoveryBlob(ctx, r, w.Store, destination, op.Before.BlobID); err != nil {
					return RewrittenRecovery{}, err
				}
				m.BeforeStored[i] = op.Before.BlobID
			}
		}
		if m.Post[i].Exists {
			actual, err := blobFingerprint(ctx, r, w.Store, m.Post[i].BlobID, m.Post[i].GitMode, m.Post[i].Permissions)
			if err != nil {
				return RewrittenRecovery{}, err
			}
			if actual != m.Post[i] {
				return RewrittenRecovery{}, fmt.Errorf("restore postimage %q differs from anchor", op.Path)
			}
			oldFile, _, err := history.SnapshotFileFromStore(ctx, r, w, w.Store, p.SourceWorkspaceID, oldSource, op.Path)
			if err != nil {
				return RewrittenRecovery{}, err
			}
			if oldFile.ID != op.SourceBlob || oldFile.Mode != op.SourceMode {
				return RewrittenRecovery{}, fmt.Errorf("restore source %q differs from history", op.Path)
			}
			newFile, digest, err := history.SnapshotFileFromStore(ctx, r, w, destination, p.SourceWorkspaceID, p.SourceSnapshot, op.Path)
			if err != nil {
				return RewrittenRecovery{}, err
			}
			if newFile.Mode != op.SourceMode || isSensitive != (digest != "") {
				return RewrittenRecovery{}, fmt.Errorf("migrated restore source %q differs from policy", op.Path)
			}
			if isSensitive {
				identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: p.SourceWorkspaceID, Path: op.Path, Mode: op.SourceMode}
				m.PostStored[i] = newFile.ID
				m.Post[i], err = protectedBlobFingerprint(ctx, r, destination, newFile.ID, key, identity, m.Post[i].Permissions)
				if err != nil {
					return RewrittenRecovery{}, err
				}
				if m.Post[i].KeyedDigest != digest {
					return RewrittenRecovery{}, fmt.Errorf("migrated restore postimage %q failed authentication", op.Path)
				}
				op.SourceBlob = newFile.ID
				op.SourceDigest = digest
			} else {
				if newFile.ID != op.SourceBlob {
					return RewrittenRecovery{}, errors.New("public restore source changed during migration")
				}
				if err := copyRecoveryBlob(ctx, r, w.Store, destination, op.SourceBlob); err != nil {
					return RewrittenRecovery{}, err
				}
				m.PostStored[i] = op.SourceBlob
			}
		}
	}
	if protected > 0 {
		j.SchemaVersion = 2
		m.SchemaVersion = 2
		p.EncryptedKeyFingerprint = policy.KeyFingerprint
	} else {
		m.BeforeStored, m.PostStored = nil, nil
	}
	newCommit, err := anchor(ctx, r, destination, *m, nil)
	if err != nil {
		return RewrittenRecovery{}, err
	}
	j.RecoverySnapshot = newCommit
	updated, err := encode(j)
	if err != nil {
		return RewrittenRecovery{}, err
	}
	newContext := w
	newContext.Store = destination
	newContext.WorkspaceID = identity.Destination
	if err := validateJournalWithLiveState(ctx, r, newContext, p.Selection, m.ConfigDigest, j.ID, j, false); err != nil {
		return RewrittenRecovery{}, fmt.Errorf("migrated restore anchor is invalid: %w", err)
	}
	return RewrittenRecovery{RecoveryMigrationIdentity: identity, NewCommit: newCommit, Journal: updated}, nil
}

// VerifyMigratedRecovery authenticates a staged completed recovery record and
// its stored images before the encrypted store is published or resumed.
func VerifyMigratedRecovery(ctx context.Context, r *gitexec.Runner, w workspace.Context, store string, data []byte, policy secureblob.Policy, key []byte) error {
	j, err := decodeMigrationJournal(data)
	if err != nil {
		return err
	}
	if j.Outcome == "" || j.Manifest.Plan.RepositoryID != w.RepositoryID {
		return errors.New("invalid completed migrated restore journal")
	}
	w.Store = store
	w.WorkspaceID = j.Manifest.Plan.DestinationWorkspaceID
	if err := validateJournalWithLiveState(ctx, r, w, j.Manifest.Plan.Selection, j.Manifest.ConfigDigest, j.ID, j, false); err != nil {
		return err
	}
	if j.SchemaVersion == 2 && j.Manifest.Plan.EncryptedKeyFingerprint != policy.KeyFingerprint {
		return errors.New("migrated restore key differs from encryption policy")
	}
	for i, op := range j.Manifest.Plan.Operations {
		if op.Encrypted != policy.Covers(op.Path) {
			return fmt.Errorf("migrated restore policy differs for %q", op.Path)
		}
		for imageIndex, image := range []Fingerprint{op.Before, j.Manifest.Post[i]} {
			if !image.Exists {
				continue
			}
			stored := image.BlobID
			if j.SchemaVersion == 2 {
				stored = j.Manifest.BeforeStored[i]
				if imageIndex == 1 {
					stored = j.Manifest.PostStored[i]
				}
			}
			var actual Fingerprint
			if op.Encrypted {
				workspaceID := w.WorkspaceID
				if imageIndex == 1 {
					workspaceID = j.Manifest.Plan.SourceWorkspaceID
				}
				identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: workspaceID, Path: op.Path, Mode: image.GitMode}
				actual, err = protectedBlobFingerprint(ctx, r, store, stored, key, identity, image.Permissions)
			} else {
				actual, err = blobFingerprint(ctx, r, store, stored, image.GitMode, image.Permissions)
			}
			if err != nil {
				return err
			}
			if actual != image {
				return fmt.Errorf("migrated restore image %q differs from recovery evidence", op.Path)
			}
		}
	}
	return nil
}

func copyRecoveryBlob(ctx context.Context, r *gitexec.Runner, source, destination, original string) error {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := r.HistoryBlobTo(ctx, source, original, writer)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	out, writeErr := r.History(ctx, destination, reader, "hash-object", "-w", "--stdin", "--no-filters")
	_ = reader.Close()
	readErr := <-done
	if readErr != nil {
		return readErr
	}
	if writeErr != nil {
		return writeErr
	}
	if strings.TrimSpace(string(out)) != original {
		return errors.New("public recovery blob changed during migration")
	}
	return nil
}

func encryptRecoveryBlob(ctx context.Context, r *gitexec.Runner, source, destination, original string, key []byte, identity secureblob.Identity) (string, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := r.HistoryBlobTo(ctx, source, original, writer)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	id, encryptErr := secureblob.EncryptGitBlob(ctx, r, destination, reader, key, identity)
	_ = reader.Close()
	readErr := <-done
	if readErr != nil {
		return "", readErr
	}
	return id, encryptErr
}
