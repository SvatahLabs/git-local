package configtx

import (
	"errors"
	"fmt"
	"os"
	"path"

	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// SeedListError reports a seed journal whose file list, kept beside it
// (seedFilesPath), is missing or doesn't match the journal. Without the list
// the seed can be neither resumed nor rolled back, so the message names the
// list and the one command that still works.
type SeedListError struct {
	ID  string
	Err error
}

func (e *SeedListError) Error() string {
	problem := "is missing"
	if !errors.Is(e.Err, os.ErrNotExist) {
		problem = "can't be used (" + e.Err.Error() + ")"
	}
	return fmt.Sprintf("the file list of seed transaction %s (%s in the common Git directory) %s; without it the seed can be neither resumed nor rolled back. If you have a copy of the list, put it back; otherwise run git local recover %s --abandon, which moves the journal into the quarantine and leaves the seeded files as they are", e.ID, path.Join("gitlocal", seedFilesPath(e.ID)), problem, e.ID)
}

func (e *SeedListError) Unwrap() error { return e.Err }

// Is makes the error a conflict: recovery needs the user (exit code 3).
func (e *SeedListError) Is(target error) bool { return target == storage.ErrConflict }

// abandonSeedWithoutList abandons the seed journal name (pending or
// completed), j as decoded without its list, whose file list is missing or
// damaged: the journal, and the damaged list if any, move into the
// quarantine, so nothing in transactions/ names the list any more. Without
// the list a pending seed's staging files can't be recognized, so they are
// left for doctor in the seed's destination worktree, which reports any that
// remain.
func (s *session) abandonSeedWithoutList(id, name string, j seedJournal, listErr error, result AbandonResult) (AbandonResult, error) {
	result.Kind = "seed"
	q, err := NewQuarantine(s.meta, id, "seed")
	if err != nil {
		return result, err
	}
	if entry, moved, err := q.QuarantineSeedFiles(s.meta, id, "file list that doesn't match its seed journal"); err != nil {
		return result, err
	} else if moved {
		result.Quarantined = append(result.Quarantined, entry)
	}
	entry, err := q.QuarantineJournal(s.meta, name, "seed journal whose file list is missing or damaged: "+listErr.Error())
	if err != nil {
		return result, err
	}
	result.Quarantined = append(result.Quarantined, entry)
	result.Quarantine = q.Directory(s.w.Common)
	// A completed seed left no staging files.
	if name == seedPath(id) {
		where := "the seed's destination worktree"
		var record workspace.Record
		if j.WorkspaceID == s.w.WorkspaceID {
			where = s.w.Root
		} else if storage.ValidUUID(j.WorkspaceID) && s.meta.ReadJSON(path.Join("workspaces", j.WorkspaceID, "workspace.json"), &record) == nil && record.Root != "" {
			where = record.Root
		}
		result.Warnings = append(result.Warnings, "without its file list the seed's staging files (.gitlocal-tmp-*, which Git ignores) couldn't be checked; git local doctor run in "+where+" lists any that are left")
	}
	return result, nil
}

// CheckSeedList returns a *SeedListError when the seed journal name in the
// private state directory meta keeps its file list beside it and the list is
// missing or doesn't match; other problems are left to the journal's readers.
func CheckSeedList(meta *storage.Dir, name string) error {
	_, err := readSeed(meta, name)
	var listErr *SeedListError
	if errors.As(err, &listErr) {
		return listErr
	}
	return nil
}
