package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/overlay"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type StagedInfo struct {
	RepositoryID     string
	WorkspaceID      string
	Workspaces       []string
	ArchivedOverlays int
}

// ValidateStaged checks authenticated extracted state before it can be
// adopted by a checkout. It does not modify the destination repository.
func ValidateStaged(ctx context.Context, r *gitexec.Runner, stage string) (StagedInfo, error) {
	var result StagedInfo
	root, err := storage.OpenDir(stage)
	if err != nil {
		return result, err
	}
	defer root.Close()
	stageEntries, err := root.ReadDir("")
	if err != nil {
		return result, err
	}
	if len(stageEntries) != 2 {
		return result, errors.New("backup stage must contain only identity and private state")
	}
	for _, entry := range stageEntries {
		if entry.Name() != "identity.json" && entry.Name() != "state" {
			return result, errors.New("backup stage contains unknown top-level data")
		}
	}
	var identity workspace.Identity
	if err := root.ReadJSON("identity.json", &identity); err != nil {
		return result, err
	}
	statePath := filepath.Join(stage, "state")
	state, err := storage.OpenDir(statePath)
	if err != nil {
		return result, err
	}
	defer state.Close()
	allowed := map[string]bool{"repository.json": true, "history.git": true, "locks": true, "transactions": true, "workspaces": true, "hook-install.json": true, "overlays": true, "overlay-filter.gitconfig": true, "overlay-install.json": true, "encryption.json": true}
	topEntries, err := state.ReadDir("")
	if err != nil {
		return result, err
	}
	for _, entry := range topEntries {
		if !allowed[entry.Name()] {
			return result, fmt.Errorf("backup contains unknown private state %q", entry.Name())
		}
	}
	var repository workspace.Repository
	if err := state.ReadJSON("repository.json", &repository); err != nil {
		return result, err
	}
	if repository.SchemaVersion != workspace.SchemaVersion || !storage.ValidUUID(repository.RepositoryID) || identity.SchemaVersion != workspace.SchemaVersion || identity.RepositoryID != repository.RepositoryID || !storage.ValidUUID(identity.WorkspaceID) {
		return result, errors.New("backup repository or workspace identity mismatch")
	}
	result.RepositoryID, result.WorkspaceID = repository.RepositoryID, identity.WorkspaceID
	if err := gitexec.ValidateStore(filepath.Join(statePath, "history.git")); err != nil {
		return result, err
	}
	if _, err := r.History(ctx, filepath.Join(statePath, "history.git"), nil, "fsck", "--full", "--no-reflogs"); err != nil {
		return result, fmt.Errorf("invalid backed-up history store: %w", err)
	}
	transactions, err := state.ReadDir("transactions")
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	for _, entry := range transactions {
		if entry.Name() != "completed" {
			return result, fmt.Errorf("backup contains pending transaction %q", entry.Name())
		}
	}
	entries, err := state.ReadDir("workspaces")
	if err != nil {
		return result, err
	}
	mainFound := false
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !storage.ValidUUID(id) {
			return result, errors.New("invalid backed-up workspace directory")
		}
		var record workspace.Record
		if err := state.ReadJSON(filepath.Join("workspaces", id, "workspace.json"), &record); err != nil {
			return result, err
		}
		if record.SchemaVersion != workspace.SchemaVersion || record.WorkspaceID != id || record.State != "active" && record.State != "retained" || !filepath.IsAbs(record.Root) || !filepath.IsAbs(record.Admin) {
			return result, fmt.Errorf("invalid backed-up workspace %s", id)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err != nil {
			return result, fmt.Errorf("invalid backed-up workspace timestamp %s", id)
		}
		var selected selection.State
		if err := state.ReadJSON(filepath.Join("workspaces", id, "selection.json"), &selected); err != nil {
			return result, err
		}
		if _, err := selection.Canonical(selected); err != nil {
			return result, err
		}
		if id == identity.WorkspaceID {
			mainFound = record.State == "active"
		}
		result.Workspaces = append(result.Workspaces, id)
	}
	if !mainFound {
		return result, errors.New("backup source workspace is not active")
	}
	manifests, err := overlay.ValidateBackupRecords(state)
	if err != nil {
		return result, err
	}
	known := make(map[string]bool, len(result.Workspaces))
	for _, id := range result.Workspaces {
		known[id] = true
	}
	for _, manifest := range manifests {
		if !known[manifest.WorkspaceID] {
			return result, errors.New("backed-up overlay has no workspace record")
		}
		if manifest.State != "removed" {
			result.ArchivedOverlays++
		}
	}
	return result, nil
}
