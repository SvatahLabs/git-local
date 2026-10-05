package encryption

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/restore"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type migrationJournal struct {
	SchemaVersion  int               `json:"schemaVersion"`
	Kind           string            `json:"kind"`
	ID             string            `json:"id"`
	RepositoryID   string            `json:"repositoryId"`
	Policy         secureblob.Policy `json:"policy"`
	OldHeads       map[string]string `json:"oldHeads"`
	NewHeads       map[string]string `json:"newHeads"`
	NewRecovery    map[string]string `json:"newRecovery,omitempty"`
	SeedUpdates    []recordUpdate    `json:"seedUpdates,omitempty"`
	RestoreUpdates []recordUpdate    `json:"restoreUpdates,omitempty"`
}

type recordUpdate struct {
	Name      string `json:"name"`
	OldDigest string `json:"oldDigest"`
	NewDigest string `json:"newDigest"`
}

type migrationFault func(string) error

func stageName(id string) string   { return "history-encrypt-" + id + ".git" }
func backupName(id string) string  { return "history-plain-" + id + ".git" }
func recordsName(id string) string { return "history-records-" + id }
func journalName(id string) string { return "transactions/encrypt-" + id + ".json" }

// Migrate rewrites existing user histories into an encrypted store and swaps it
// under an exclusive common lock. Interrupted publication is resumed or rolled
// back by RecoverMigration using the durable journal.
func Migrate(ctx context.Context, r *gitexec.Runner, cwd, keyFile string, paths []string) (string, error) {
	return migrate(ctx, r, cwd, keyFile, paths, nil)
}

func migrate(ctx context.Context, r *gitexec.Runner, cwd, keyFile string, paths []string, fault migrationFault) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("at least one sensitive path is required")
	}
	id, err := storage.UUID()
	if err != nil {
		return "", err
	}
	_, err = configtx.WithSelection(ctx, r, cwd, func(w workspace.Context, state selection.State, recheck func() error) error {
		policy, key, err := migrationPolicy(ctx, r, cwd, w, state, keyFile, paths)
		if err != nil {
			return err
		}
		common, err := storage.OpenDir(w.Common)
		if err != nil {
			return err
		}
		defer common.Close()
		if _, exists, err := secureblob.ReadLocked(common, w.Root, w.Common); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: sensitive-file encryption is already enabled", storage.ErrConflict)
		}
		meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
		if err != nil {
			return err
		}
		defer meta.Close()
		seedJournals, restoreJournals, err := migrationJournalsSupported(meta)
		if err != nil {
			return err
		}
		allowedRecovery, err := inspectRecoveryJournals(ctx, r, w, meta, restoreJournals)
		if err != nil {
			return err
		}
		stage := filepath.Join(w.Common, "gitlocal", stageName(id))
		if err := r.InitBare(ctx, stage); err != nil {
			return err
		}
		prepared := false
		defer func() {
			if !prepared {
				_ = removeOwnedStore(stage)
				_ = removeOwnedStore(filepath.Join(w.Common, "gitlocal", recordsName(id)))
			}
		}()
		oldHeads, err := storeHeads(ctx, r, w.Store)
		if err != nil {
			return err
		}
		rewritten, err := history.RewritePlaintextStore(ctx, r, w, stage, policy, key, allowedRecovery)
		if err != nil {
			return err
		}
		seedUpdates, err := prepareSeedUpdates(ctx, r, w, meta, id, stage, seedJournals, rewritten, policy)
		if err != nil {
			return err
		}
		restoreUpdates, newRecovery, err := prepareRestoreUpdates(ctx, r, w, meta, id, stage, restoreJournals, rewritten, policy, key)
		if err != nil {
			return err
		}
		if err := history.VerifyEncryptedStore(ctx, r, w, stage, policy, key, rewritten.Heads, newRecovery); err != nil {
			return err
		}
		if err := verifyRecoveryUpdates(ctx, r, w, meta, id, stage, restoreUpdates, policy, key); err != nil {
			return err
		}
		if err := recheck(); err != nil {
			return err
		}
		if err := matchHeads(ctx, r, w.Store, oldHeads); err != nil {
			return err
		}
		journal := migrationJournal{SchemaVersion: 1, Kind: "encrypt", ID: id, RepositoryID: w.RepositoryID, Policy: policy, OldHeads: oldHeads, NewHeads: rewritten.Heads, NewRecovery: newRecovery, SeedUpdates: seedUpdates, RestoreUpdates: restoreUpdates}
		if err := meta.WriteJSON(journalName(id), journal); err != nil {
			return err
		}
		prepared = true
		if err := migratePoint(ctx, fault, "journal"); err != nil {
			return stoppedMigration(id, err)
		}
		if err := publishMigration(ctx, r, w, common, meta, journal, fault); err != nil {
			return stoppedMigration(id, err)
		}
		return nil
	})
	return id, err
}

func migrationJournalsSupported(meta *storage.Dir) ([]string, []string, error) {
	entries, err := meta.ReadDir("transactions/completed")
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	seeds, restores := []string{}, []string{}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			return nil, nil, fmt.Errorf("%w: unsupported completed transaction %q", storage.ErrConflict, name)
		}
		kind, id, _ := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
		if !storage.ValidUUID(id) {
			return nil, nil, fmt.Errorf("%w: invalid completed transaction %q", storage.ErrConflict, name)
		}
		switch kind {
		case "config":
		case "seed":
			seeds = append(seeds, name)
		case "restore":
			restores = append(restores, name)
		default:
			return nil, nil, fmt.Errorf("%w: completed transaction %q requires a migration handler", storage.ErrConflict, name)
		}
	}
	return seeds, restores, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func inspectRecoveryJournals(ctx context.Context, r *gitexec.Runner, w workspace.Context, meta *storage.Dir, names []string) (map[string]string, error) {
	result := map[string]string{}
	for _, name := range names {
		data, err := meta.ReadFile("transactions/completed/"+name, 16<<20)
		if err != nil {
			return nil, err
		}
		identity, err := restore.InspectCompletedRecoveryForMigration(ctx, r, w, data)
		if err != nil {
			return nil, fmt.Errorf("%w: completed restore journal %q cannot be migrated: %v", storage.ErrConflict, name, err)
		}
		if name != "restore-"+identity.ID+".json" || result[identity.Ref] != "" {
			return nil, fmt.Errorf("%w: completed restore journal identity differs", storage.ErrConflict)
		}
		result[identity.Ref] = identity.OldCommit
	}
	return result, nil
}

func prepareRestoreUpdates(ctx context.Context, r *gitexec.Runner, w workspace.Context, meta *storage.Dir, id, stage string, names []string, rewritten history.MigrationResult, policy secureblob.Policy, key []byte) ([]recordUpdate, map[string]string, error) {
	newRecovery := map[string]string{}
	if len(names) == 0 {
		return nil, newRecovery, nil
	}
	if err := meta.MkdirAll(recordsName(id)); err != nil {
		return nil, nil, err
	}
	updates := make([]recordUpdate, 0, len(names))
	for _, name := range names {
		old, err := meta.ReadFile("transactions/completed/"+name, 16<<20)
		if err != nil {
			return nil, nil, err
		}
		converted, err := restore.RewriteCompletedRecovery(ctx, r, w, stage, old, policy, key, func(original string) (string, error) {
			mapped := rewritten.Snapshots[original]
			if mapped == "" {
				return "", fmt.Errorf("%w: restore snapshot %s is not reachable", storage.ErrConflict, original)
			}
			return mapped, nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("completed restore journal %q: %w", name, err)
		}
		if name != "restore-"+converted.ID+".json" || newRecovery[converted.Ref] != "" {
			return nil, nil, errors.New("duplicate migrated restore recovery ref")
		}
		if err := meta.AtomicWrite(filepath.Join(recordsName(id), name), converted.Journal, 0600); err != nil {
			return nil, nil, err
		}
		updates = append(updates, recordUpdate{Name: name, OldDigest: digestBytes(old), NewDigest: digestBytes(converted.Journal)})
		newRecovery[converted.Ref] = converted.NewCommit
	}
	return updates, newRecovery, nil
}

func prepareSeedUpdates(ctx context.Context, r *gitexec.Runner, w workspace.Context, meta *storage.Dir, id, stage string, names []string, rewritten history.MigrationResult, policy secureblob.Policy) ([]recordUpdate, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if err := meta.MkdirAll(recordsName(id)); err != nil {
		return nil, err
	}
	updates := make([]recordUpdate, 0, len(names))
	for _, name := range names {
		old, err := meta.ReadFile("transactions/completed/"+name, 1<<20)
		if err != nil {
			return nil, err
		}
		updated, err := configtx.RewriteCompletedSeedJournal(old, w.RepositoryID, policy.KeyFingerprint,
			func(_, oldSnapshot string) (string, error) {
				newSnapshot := rewritten.Snapshots[oldSnapshot]
				if newSnapshot == "" {
					return "", fmt.Errorf("%w: seed source snapshot %s is not reachable", storage.ErrConflict, oldSnapshot)
				}
				return newSnapshot, nil
			},
			func(source, oldSnapshot, newSnapshot string, file configtx.SeedFile) (string, string, error) {
				before, _, err := history.SnapshotFileFromStore(ctx, r, w, w.Store, source, oldSnapshot, file.Path)
				if err != nil {
					return "", "", err
				}
				if before.ID != file.Blob || before.Mode != file.Mode {
					return "", "", errors.New("completed seed file differs from source history")
				}
				after, digest, err := history.SnapshotFileFromStore(ctx, r, w, stage, source, newSnapshot, file.Path)
				if err != nil {
					return "", "", err
				}
				if after.Mode != file.Mode || policy.Covers(file.Path) != (digest != "") {
					return "", "", errors.New("migrated seed file differs from encryption policy")
				}
				return after.ID, digest, nil
			})
		if err != nil {
			return nil, fmt.Errorf("completed seed journal %q: %w", name, err)
		}
		if err := meta.AtomicWrite(filepath.Join(recordsName(id), name), updated, 0600); err != nil {
			return nil, err
		}
		updates = append(updates, recordUpdate{Name: name, OldDigest: digestBytes(old), NewDigest: digestBytes(updated)})
	}
	return updates, nil
}

func applyRecordUpdates(meta *storage.Dir, j migrationJournal) error {
	for _, update := range append(append([]recordUpdate{}, j.SeedUpdates...), j.RestoreUpdates...) {
		current, err := meta.ReadFile("transactions/completed/"+update.Name, 16<<20)
		if err != nil {
			return err
		}
		actual := digestBytes(current)
		if actual == update.NewDigest {
			continue
		}
		if actual != update.OldDigest {
			return fmt.Errorf("%w: completed journal %q changed during migration", storage.ErrConflict, update.Name)
		}
		prepared, err := meta.ReadFile(filepath.Join(recordsName(j.ID), update.Name), 16<<20)
		if err != nil || digestBytes(prepared) != update.NewDigest {
			return fmt.Errorf("prepared journal %q changed: %v", update.Name, err)
		}
		if err := meta.AtomicWrite("transactions/completed/"+update.Name, prepared, 0600); err != nil {
			return err
		}
	}
	return nil
}

func verifyRecoveryUpdates(ctx context.Context, r *gitexec.Runner, w workspace.Context, meta *storage.Dir, id, store string, updates []recordUpdate, policy secureblob.Policy, key []byte) error {
	for _, update := range updates {
		live, err := meta.ReadFile("transactions/completed/"+update.Name, 16<<20)
		if err != nil {
			return err
		}
		var prepared []byte
		switch digestBytes(live) {
		case update.NewDigest:
			prepared = live
		case update.OldDigest:
			prepared, err = meta.ReadFile(filepath.Join(recordsName(id), update.Name), 16<<20)
			if err != nil {
				return err
			}
			if digestBytes(prepared) != update.NewDigest {
				return fmt.Errorf("prepared recovery journal %q changed", update.Name)
			}
		default:
			return fmt.Errorf("%w: completed recovery journal %q changed", storage.ErrConflict, update.Name)
		}
		if err := restore.VerifyMigratedRecovery(ctx, r, w, store, prepared, policy, key); err != nil {
			return fmt.Errorf("completed recovery journal %q: %w", update.Name, err)
		}
	}
	return nil
}

func migrationPolicy(ctx context.Context, r *gitexec.Runner, cwd string, w workspace.Context, state selection.State, keyFile string, paths []string) (secureblob.Policy, []byte, error) {
	engine, err := selection.New(r, w.Root, w.Admin)
	if err != nil {
		return secureblob.Policy{}, nil, err
	}
	defer engine.Close()
	normalized := make([]string, 0, len(paths))
	for _, arg := range paths {
		path, err := engine.Normalize(cwd, arg)
		if err != nil {
			return secureblob.Policy{}, nil, err
		}
		if !state.Selected(path) {
			return secureblob.Policy{}, nil, fmt.Errorf("%w: %q is not selected for local history", storage.ErrConflict, path)
		}
		normalized = append(normalized, path)
	}
	recorded, err := history.HistoricalPaths(ctx, r, w)
	if err != nil {
		return secureblob.Policy{}, nil, err
	}
	rules, err := sensitiveRules(w.Root, state, normalized, recorded)
	if err != nil {
		return secureblob.Policy{}, nil, err
	}
	policy, err := secureblob.PolicyForKey(keyFile, w.Root, w.Common, rules)
	if err != nil {
		return secureblob.Policy{}, nil, err
	}
	roots, err := workspace.RegisteredRoots(ctx, r, &w)
	if err != nil {
		return secureblob.Policy{}, nil, err
	}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			return secureblob.Policy{}, nil, fmt.Errorf("%w: verify or prune unavailable worktree %q before migrating encryption: %v", storage.ErrConflict, root, err)
		}
		if _, err := secureblob.Canonical(policy, root, w.Common); err != nil {
			return secureblob.Policy{}, nil, err
		}
	}
	key, err := secureblob.LoadKey(policy, w.Root, w.Common)
	return policy, key, err
}

func migratePoint(ctx context.Context, fault migrationFault, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fault != nil {
		return fault(name)
	}
	return nil
}

func stoppedMigration(id string, err error) error {
	return fmt.Errorf("%w: encryption migration %s interrupted (%v); run git local encrypt recover %s --resume or --rollback", storage.ErrConflict, id, err, id)
}

func publishMigration(ctx context.Context, r *gitexec.Runner, w workspace.Context, common, meta *storage.Dir, j migrationJournal, fault migrationFault) error {
	if err := meta.Rename("history.git", backupName(j.ID)); err != nil {
		return err
	}
	if err := migratePoint(ctx, fault, "old-store-moved"); err != nil {
		return err
	}
	if err := meta.Rename(stageName(j.ID), "history.git"); err != nil {
		return err
	}
	if err := migratePoint(ctx, fault, "encrypted-store-published"); err != nil {
		return err
	}
	if err := secureblob.WriteLocked(common, j.Policy, w.Root, w.Common); err != nil {
		return err
	}
	if err := migratePoint(ctx, fault, "policy-published"); err != nil {
		return err
	}
	if err := applyRecordUpdates(meta, j); err != nil {
		return err
	}
	if err := migratePoint(ctx, fault, "record-updates"); err != nil {
		return err
	}
	if err := removeOwnedStore(filepath.Join(w.Common, "gitlocal", recordsName(j.ID))); err != nil {
		return err
	}
	if err := removeOwnedStore(filepath.Join(w.Common, "gitlocal", backupName(j.ID))); err != nil {
		return err
	}
	return meta.Remove(journalName(j.ID))
}

func storeHeads(ctx context.Context, r *gitexec.Runner, store string) (map[string]string, error) {
	data, err := r.History(ctx, store, nil, "show-ref")
	if err != nil {
		var ge *gitexec.Error
		if errors.As(err, &ge) && ge.Code == 1 {
			return map[string]string{}, nil
		}
		return nil, err
	}
	heads := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !validMigrationOID(fields[0]) {
			return nil, errors.New("invalid private store ref")
		}
		if _, old := heads[fields[1]]; old {
			return nil, errors.New("duplicate private store ref")
		}
		heads[fields[1]] = fields[0]
	}
	return heads, nil
}

func validMigrationOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

func validMigrationDigest(value string) bool {
	return len(value) == 64 && validMigrationOID(value)
}

func matchHeads(ctx context.Context, r *gitexec.Runner, store string, expected map[string]string) error {
	actual, err := storeHeads(ctx, r, store)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("%w: private history refs changed during encryption migration", storage.ErrConflict)
	}
	for ref, id := range expected {
		if actual[ref] != id {
			return fmt.Errorf("%w: private history ref %q changed", storage.ErrConflict, ref)
		}
	}
	return nil
}

func newHeadsAsRefs(heads, recovery map[string]string) map[string]string {
	result := make(map[string]string, len(heads)+len(recovery))
	for id, objectID := range heads {
		result[history.Ref(id)] = objectID
	}
	for ref, objectID := range recovery {
		result[ref] = objectID
	}
	return result
}

func removeOwnedStore(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("migration store path is not a directory")
	}
	err = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("unsafe migration store entry")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func readMigrationJournal(meta *storage.Dir, id, repositoryID string) (migrationJournal, error) {
	var j migrationJournal
	data, err := meta.ReadFile(journalName(id), 1<<20)
	if err != nil {
		return j, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return j, err
	}
	if dec.Decode(new(any)) != io.EOF || j.SchemaVersion != 1 || j.Kind != "encrypt" || j.ID != id || j.RepositoryID != repositoryID {
		return j, errors.New("invalid encryption migration journal")
	}
	if len(j.OldHeads) != len(j.NewHeads)+len(j.NewRecovery) {
		return j, errors.New("encryption migration journal has unmatched workspace heads")
	}
	for ref, old := range j.OldHeads {
		const prefix = "refs/gitlocal/workspaces/"
		if !validMigrationOID(old) {
			return j, errors.New("invalid original head in encryption migration journal")
		}
		if strings.HasPrefix(ref, "refs/gitlocal/recovery/") {
			parts := strings.Split(strings.TrimPrefix(ref, "refs/gitlocal/recovery/"), "/")
			if len(parts) != 2 || !storage.ValidUUID(parts[0]) || !storage.ValidUUID(parts[1]) || ref != restore.RecoveryRef(parts[0], parts[1]) || !validMigrationOID(j.NewRecovery[ref]) {
				return j, errors.New("invalid migrated recovery head in encryption migration journal")
			}
			continue
		}
		if !strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, "/head") {
			return j, errors.New("invalid workspace head in encryption migration journal")
		}
		workspaceID := strings.TrimSuffix(strings.TrimPrefix(ref, prefix), "/head")
		if !storage.ValidUUID(workspaceID) || ref != history.Ref(workspaceID) || !validMigrationOID(j.NewHeads[workspaceID]) {
			return j, errors.New("invalid migrated head in encryption migration journal")
		}
	}
	for _, group := range []struct {
		prefix  string
		updates []recordUpdate
	}{{"seed-", j.SeedUpdates}, {"restore-", j.RestoreUpdates}} {
		previous := ""
		for _, update := range group.updates {
			if !strings.HasPrefix(update.Name, group.prefix) || !strings.HasSuffix(update.Name, ".json") || !storage.ValidUUID(strings.TrimSuffix(strings.TrimPrefix(update.Name, group.prefix), ".json")) || update.Name <= previous || !validMigrationDigest(update.OldDigest) || !validMigrationDigest(update.NewDigest) {
				return j, errors.New("invalid completed journal update in migration")
			}
			previous = update.Name
		}
	}
	if len(j.RestoreUpdates) != len(j.NewRecovery) {
		return j, errors.New("migration recovery refs and journals differ")
	}
	for ref := range j.NewRecovery {
		matched := false
		for _, update := range j.RestoreUpdates {
			if strings.HasSuffix(ref, "/"+strings.TrimSuffix(strings.TrimPrefix(update.Name, "restore-"), ".json")) {
				matched = true
				break
			}
		}
		if !matched || j.OldHeads[ref] == "" {
			return j, errors.New("migration recovery ref has no matching completed journal")
		}
	}
	for _, update := range j.RestoreUpdates {
		id := strings.TrimSuffix(strings.TrimPrefix(update.Name, "restore-"), ".json")
		matches := 0
		for ref := range j.NewRecovery {
			if strings.HasSuffix(ref, "/"+id) {
				matches++
			}
		}
		if matches != 1 {
			return j, errors.New("completed recovery journal has no unique migrated ref")
		}
	}
	return j, nil
}

func storeExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, errors.New("migration store path is obstructed")
	}
	return true, gitexec.ValidateStore(path)
}

// RecoverMigration resolves a recorded swap. Rollback is allowed until the
// policy is published; after that only verifying and finishing cleanup is safe.
func RecoverMigration(ctx context.Context, r *gitexec.Runner, cwd, id, direction string) error {
	if !storage.ValidUUID(id) || direction != "resume" && direction != "rollback" {
		return errors.New("encrypt recover requires a transaction UUID and --resume or --rollback")
	}
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return err
	}
	defer common.Close()
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		return err
	}
	defer lock.Close()
	var repo workspace.Repository
	if err := common.ReadJSON("gitlocal/repository.json", &repo); err != nil {
		return err
	}
	if repo.SchemaVersion != workspace.SchemaVersion || !storage.ValidUUID(repo.RepositoryID) {
		return errors.New("invalid repository identity during encryption recovery")
	}
	admin, err := storage.OpenDir(w.Admin)
	if err != nil {
		return err
	}
	var identity workspace.Identity
	identityErr := admin.ReadJSON("gitlocal-workspace.json", &identity)
	admin.Close()
	if identityErr != nil {
		return identityErr
	}
	if identity.SchemaVersion != workspace.SchemaVersion || identity.RepositoryID != repo.RepositoryID || !storage.ValidUUID(identity.WorkspaceID) {
		return errors.New("invalid workspace identity during encryption recovery")
	}
	w.RepositoryID = repo.RepositoryID
	w.WorkspaceID = identity.WorkspaceID
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return err
	}
	defer meta.Close()
	j, err := readMigrationJournal(meta, id, repo.RepositoryID)
	if err != nil {
		return err
	}
	base := filepath.Join(w.Common, "gitlocal")
	stage, backup := filepath.Join(base, stageName(id)), filepath.Join(base, backupName(id))
	liveExists, err := storeExists(w.Store)
	if err != nil {
		return err
	}
	stageExists, err := storeExists(stage)
	if err != nil {
		return err
	}
	backupExists, err := storeExists(backup)
	if err != nil {
		return err
	}
	policy, published, err := secureblob.ReadLocked(common, w.Root, w.Common)
	if err != nil {
		return err
	}
	if published && (policy.KeyFingerprint != j.Policy.KeyFingerprint || policy.KeyFile != j.Policy.KeyFile) {
		return errors.New("published policy differs from encryption migration journal")
	}
	if direction == "resume" || published {
		if _, err := secureblob.Canonical(j.Policy, w.Root, w.Common); err != nil {
			return err
		}
	}
	if published && direction == "rollback" {
		return errors.New("encryption policy is published; resume cleanup instead of rolling back")
	}
	if backupExists {
		if err := matchHeads(ctx, r, backup, j.OldHeads); err != nil {
			return err
		}
	} else if !published && liveExists {
		if err := matchHeads(ctx, r, w.Store, j.OldHeads); err != nil && !(len(j.NewHeads)+len(j.NewRecovery) > 0 && matchHeads(ctx, r, w.Store, newHeadsAsRefs(j.NewHeads, j.NewRecovery)) == nil) {
			return err
		}
	}
	if direction == "rollback" {
		if backupExists {
			if liveExists {
				if err := matchHeads(ctx, r, w.Store, newHeadsAsRefs(j.NewHeads, j.NewRecovery)); err != nil {
					return err
				}
				if stageExists {
					return errors.New("both encrypted stores exist during rollback")
				}
				if err := meta.Rename("history.git", stageName(id)); err != nil {
					return err
				}
			}
			if err := meta.Rename(backupName(id), "history.git"); err != nil {
				return err
			}
		} else if !liveExists {
			return errors.New("original history store is missing")
		}
		if err := removeOwnedStore(stage); err != nil {
			return err
		}
		if err := removeOwnedStore(filepath.Join(base, recordsName(id))); err != nil {
			return err
		}
		return meta.Remove(journalName(id))
	}
	key, err := secureblob.LoadKey(j.Policy, w.Root, w.Common)
	if err != nil {
		return err
	}
	if !published {
		if backupExists {
			if stageExists && !liveExists {
				if err := history.VerifyEncryptedStore(ctx, r, *w, stage, j.Policy, key, j.NewHeads, j.NewRecovery); err != nil {
					return err
				}
				if err := meta.Rename(stageName(id), "history.git"); err != nil {
					return err
				}
				liveExists = true
			} else if !liveExists {
				return errors.New("encrypted history store is missing")
			}
		} else {
			if !liveExists || !stageExists {
				return errors.New("migration has no prepared encrypted store; roll back")
			}
			if err := matchHeads(ctx, r, w.Store, j.OldHeads); err != nil {
				return err
			}
			if err := history.VerifyEncryptedStore(ctx, r, *w, stage, j.Policy, key, j.NewHeads, j.NewRecovery); err != nil {
				return err
			}
			if err := meta.Rename("history.git", backupName(id)); err != nil {
				return err
			}
			if err := meta.Rename(stageName(id), "history.git"); err != nil {
				return err
			}
			backupExists, liveExists = true, true
		}
		if err := history.VerifyEncryptedStore(ctx, r, *w, w.Store, j.Policy, key, j.NewHeads, j.NewRecovery); err != nil {
			return err
		}
		if err := verifyRecoveryUpdates(ctx, r, *w, meta, j.ID, w.Store, j.RestoreUpdates, j.Policy, key); err != nil {
			return err
		}
		if err := secureblob.WriteLocked(common, j.Policy, w.Root, w.Common); err != nil {
			return err
		}
	} else {
		if !liveExists {
			return errors.New("published encrypted history store is missing")
		}
		if err := history.VerifyEncryptedStore(ctx, r, *w, w.Store, j.Policy, key, j.NewHeads, j.NewRecovery); err != nil {
			return err
		}
		if err := verifyRecoveryUpdates(ctx, r, *w, meta, j.ID, w.Store, j.RestoreUpdates, j.Policy, key); err != nil {
			return err
		}
	}
	if err := applyRecordUpdates(meta, j); err != nil {
		return err
	}
	if err := removeOwnedStore(filepath.Join(base, recordsName(id))); err != nil {
		return err
	}
	if backupExists {
		if err := removeOwnedStore(backup); err != nil {
			return err
		}
	}
	return meta.Remove(journalName(id))
}
