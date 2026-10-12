package configtx

import (
	"errors"
	"fmt"

	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
)

// SeedFileMigration maps a completed seed journal's source file to the same
// file in a rewritten snapshot. It must authenticate the old and new objects.
type SeedFileMigration func(source, oldSnapshot, newSnapshot string, file SeedFile) (newBlob, plainDigest string, err error)
type SeedSnapshotMigration func(source, oldSnapshot string) (newSnapshot string, err error)

// RewriteCompletedSeedJournal updates only historical object references in a
// completed journal. files reads a file list kept beside the journal
// (ReadSeedFiles). The caller stages the returned journal, and the file list
// to write beside it when one is returned, until the encrypted store and policy
// have been published; the list is written before the journal (WriteSeedFiles).
// Pending journals are never rewritten.
func RewriteCompletedSeedJournal(data []byte, files func(id string) ([]byte, error), repositoryID, keyFingerprint string, mapSnapshot SeedSnapshotMigration, mapFile SeedFileMigration) ([]byte, []byte, error) {
	j, err := decodeSeed(data, files)
	if err != nil {
		return nil, nil, err
	}
	// Schema 2 records come from encrypted history, which only a policy
	// correction with the same key rewrites.
	if !storage.ValidUUID(j.WorkspaceID) || j.Outcome == "" || !(j.SchemaVersion == 1 && j.KeyFingerprint == "" || j.SchemaVersion == 2 && j.KeyFingerprint == keyFingerprint) {
		return nil, nil, errors.New("unsupported completed seed journal for encryption migration")
	}
	if err := validateSeed(j, repositoryID, j.WorkspaceID); err != nil {
		return nil, nil, err
	}
	oldSnapshot := j.Snapshot
	newSnapshot, err := mapSnapshot(j.Source, oldSnapshot)
	if err != nil {
		return nil, nil, err
	}
	if !oidValid(newSnapshot) {
		return nil, nil, errors.New("invalid migrated seed snapshot ID")
	}
	protected := 0
	for i, file := range j.Files {
		blob, digest, err := mapFile(j.Source, oldSnapshot, newSnapshot, file)
		if err != nil {
			return nil, nil, fmt.Errorf("seed file %q: %w", file.Path, err)
		}
		if !oidValid(blob) || digest != "" && !validSeedDigest(digest) {
			return nil, nil, errors.New("invalid migrated seed file evidence")
		}
		if file.PlainDigest != "" && digest == "" {
			return nil, nil, errors.New("migrated seed file lost its encryption")
		}
		j.Files[i].Blob = blob
		j.Files[i].PlainDigest = digest
		if digest != "" {
			protected++
		}
	}
	j.Snapshot = newSnapshot
	if protected > 0 {
		if !validSeedDigest(keyFingerprint) {
			return nil, nil, errors.New("invalid migrated seed key fingerprint")
		}
		j.SchemaVersion = 2
		j.KeyFingerprint = keyFingerprint
	}
	if err := validateSeed(j, repositoryID, j.WorkspaceID); err != nil {
		return nil, nil, err
	}
	// A list kept beside the journal is written again, with a new digest, over
	// the one that names the old objects.
	if j.FilesDigest != "" {
		j.FilesDigest = ""
		return encodeSeedBeside(j)
	}
	return encodeSeed(j)
}

// VerifyCompletedSeedForBackup validates a completed seed record against the
// encrypted source snapshot before a backup is adopted in another clone.
// files reads a file list kept beside the journal (ReadSeedFiles).
func VerifyCompletedSeedForBackup(data []byte, files func(id string) ([]byte, error), repositoryID, name string, policy secureblob.Policy, sourceFile func(source, snapshot, path string) (blob, mode, digest string, err error)) error {
	j, err := decodeSeed(data, files)
	if err != nil {
		return err
	}
	if j.Outcome == "" || name != "seed-"+j.ID+".json" {
		return errors.New("invalid completed seed journal identity")
	}
	if err := validateSeed(j, repositoryID, j.WorkspaceID); err != nil {
		return err
	}
	protected := 0
	for _, file := range j.Files {
		blob, mode, digest, err := sourceFile(j.Source, j.Snapshot, file.Path)
		if err != nil {
			return err
		}
		if blob != file.Blob || mode != file.Mode || digest != file.PlainDigest || policy.Covers(file.Path) != (digest != "") {
			return fmt.Errorf("completed seed file %q differs from encrypted history", file.Path)
		}
		if digest != "" {
			protected++
		}
	}
	if protected > 0 && (j.SchemaVersion != 2 || j.KeyFingerprint != policy.KeyFingerprint) || protected == 0 && j.SchemaVersion != 1 {
		return errors.New("completed seed encryption evidence differs from policy")
	}
	return nil
}
