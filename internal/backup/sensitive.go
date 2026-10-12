package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/restore"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func prepareSensitiveBackup(ctx context.Context, r *gitexec.Runner, w workspace.Context, statePath string, state *storage.Dir, fileKey []string) error {
	if len(fileKey) > 1 {
		return errors.New("backup restore accepts only one sensitive-file key")
	}
	var policy secureblob.Policy
	if err := state.ReadJSON("encryption.json", &policy); err != nil {
		if os.IsNotExist(err) {
			if len(fileKey) != 0 {
				return errors.New("backup has no sensitive-file policy for --file-key")
			}
			return nil
		}
		return err
	}
	if len(fileKey) != 0 {
		updated, err := secureblob.RebindKeyPath(policy, fileKey[0], w.Root, w.Common)
		if err != nil {
			return fmt.Errorf("sensitive-file key cannot be rebound: %w", err)
		}
		policy = updated
		if err := state.WriteJSON("encryption.json", policy); err != nil {
			return err
		}
	} else if _, err := secureblob.LoadKey(policy, w.Root, w.Common); err != nil {
		if path, ok := secureblob.UnavailableKeyPath(err); ok {
			return fmt.Errorf("the backup's sensitive-file key %s doesn't exist here; pass --file-key with the matching external key", path)
		}
		return fmt.Errorf("sensitive-file key is unavailable; pass --file-key with the matching external key: %w", err)
	}
	return verifySensitiveBackup(ctx, r, w, statePath, state)
}

// verifySensitiveBackup authenticates every encrypted user snapshot and
// completed restore recovery image in the prepared or installed private store.
// It is repeated during interrupted adoption before publishing the identity.
func verifySensitiveBackup(ctx context.Context, r *gitexec.Runner, w workspace.Context, statePath string, state *storage.Dir) error {
	var policy secureblob.Policy
	if err := state.ReadJSON("encryption.json", &policy); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	key, err := secureblob.LoadKey(policy, w.Root, w.Common)
	if err != nil {
		return fmt.Errorf("sensitive-file key is unavailable for backup adoption: %w", err)
	}
	var repository workspace.Repository
	if err := state.ReadJSON("repository.json", &repository); err != nil {
		return err
	}
	w.RepositoryID = repository.RepositoryID
	store := filepath.Join(statePath, "history.git")
	w.Store = store
	known := map[string]bool{}
	workspaces, err := state.ReadDir("workspaces")
	if err != nil {
		return err
	}
	for _, entry := range workspaces {
		if !entry.IsDir() || !storage.ValidUUID(entry.Name()) {
			return errors.New("invalid encrypted backup workspace")
		}
		known[entry.Name()] = true
	}
	recovery := map[string]string{}
	completed, err := state.ReadDir("transactions/completed")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range completed {
		name := entry.Name()
		if strings.HasPrefix(name, "seed-") && strings.HasSuffix(name, ".json") {
			data, err := state.ReadFile(filepath.Join("transactions/completed", name), configtx.JournalLimit)
			if err != nil {
				return err
			}
			if err := configtx.VerifyCompletedSeedForBackup(data, func(id string) ([]byte, error) { return configtx.ReadSeedFiles(state, id) }, w.RepositoryID, name, policy, func(source, snapshot, path string) (string, string, string, error) {
				if !known[source] {
					return "", "", "", errors.New("seed source workspace is absent from backup")
				}
				file, digest, err := history.SnapshotFileFromStore(ctx, r, w, store, source, snapshot, path)
				return file.ID, file.Mode, digest, err
			}); err != nil {
				return fmt.Errorf("backup seed journal %q: %w", name, err)
			}
			continue
		}
		if !strings.HasPrefix(name, "restore-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := state.ReadFile(filepath.Join("transactions/completed", name), 16<<20)
		if err != nil {
			return err
		}
		var record struct {
			ID               string `json:"id"`
			RecoverySnapshot string `json:"recoverySnapshot"`
			Manifest         struct {
				Plan struct {
					DestinationWorkspaceID string `json:"destinationWorkspaceId"`
				} `json:"plan"`
			} `json:"manifest"`
		}
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		if name != "restore-"+record.ID+".json" || !known[record.Manifest.Plan.DestinationWorkspaceID] {
			return errors.New("invalid encrypted backup recovery journal identity")
		}
		if err := restore.VerifyMigratedRecovery(ctx, r, w, store, data, policy, key); err != nil {
			return fmt.Errorf("backup recovery journal %q: %w", name, err)
		}
		ref := restore.RecoveryRef(record.Manifest.Plan.DestinationWorkspaceID, record.ID)
		if recovery[ref] != "" {
			return errors.New("duplicate backup recovery journal")
		}
		recovery[ref] = record.RecoverySnapshot
	}
	heads := map[string]string{}
	refs, err := r.HistoryListing(ctx, store, nil, "show-ref")
	if err != nil {
		var gitErr *gitexec.Error
		if !errors.As(err, &gitErr) || gitErr.Code != 1 {
			return err
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return errors.New("invalid encrypted backup ref")
		}
		id := strings.TrimSuffix(strings.TrimPrefix(fields[1], "refs/gitlocal/workspaces/"), "/head")
		if fields[1] == history.Ref(id) {
			if !known[id] || heads[id] != "" {
				return errors.New("encrypted backup head has no workspace")
			}
			heads[id] = fields[0]
		}
	}
	return history.VerifyEncryptedStore(ctx, r, w, store, policy, key, heads, recovery)
}
