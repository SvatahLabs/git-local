package configtx

import (
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
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
	return fmt.Sprintf("the file list of seed transaction %s (%s in the common Git directory) %s; without it the seed can be neither resumed nor rolled back. If you have a copy of the list, put it back; otherwise run git local recover %s --abandon, which moves the journal into the quarantine and leaves the seeded files as they are", e.ID, e.List(), e.Problem(), e.ID)
}

// List is the file list's path in the common Git directory.
func (e *SeedListError) List() string { return path.Join("gitlocal", seedFilesPath(e.ID)) }

// Problem says what is wrong with the list: "is missing", "doesn't match its
// journal", or "can't be read (...)".
func (e *SeedListError) Problem() string {
	switch {
	case errors.Is(e.Err, os.ErrNotExist):
		return "is missing"
	case errors.Is(e.Err, errListMismatch):
		return "doesn't match its journal"
	}
	return "can't be read (" + e.Err.Error() + ")"
}

// errListMismatch is a seed file list whose digest isn't the journal's.
var errListMismatch = fmt.Errorf("%w: it doesn't match its journal", storage.ErrConflict)

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
	// The reasons say what was wrong, not what to do: this is the abandon.
	problem := "is missing"
	var seedErr *SeedListError
	if errors.As(listErr, &seedErr) {
		problem = seedErr.Problem()
	}
	if entry, moved, err := q.QuarantineSeedFiles(s.meta, id, "seed file list that "+problem); err != nil {
		return result, err
	} else if moved {
		result.Quarantined = append(result.Quarantined, entry)
	}
	entry, err := q.QuarantineJournal(s.meta, name, "seed journal whose file list "+problem)
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
