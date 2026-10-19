package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/exclude"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/overlay"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

const restoreJournalName = "gitlocal-backup-restore.json"

// RestoreLock is the lock, in the common Git directory, that backup restore
// and its recovery hold, after the identity lock.
const RestoreLock = "gitlocal-backup-restore.lock"

// identityLocking runs just before restore and its recovery take the
// identity lock; tests replace it to know when they wait for it.
var identityLocking = func() {}

type restoreJournal struct {
	SchemaVersion    int                `json:"schemaVersion"`
	RepositoryID     string             `json:"repositoryId"`
	WorkspaceID      string             `json:"workspaceId"`
	Prepared         string             `json:"prepared"`
	Identity         workspace.Identity `json:"identity"`
	OldExclude       []byte             `json:"oldExclude"`
	NewExclude       []byte             `json:"newExclude"`
	ExcludeExists    bool               `json:"excludeExists"`
	ExcludeMode      uint32             `json:"excludeMode"`
	ArchivedOverlays int                `json:"archivedOverlays,omitempty"`
}

// AdoptStaged installs authenticated state into an uninitialized main checkout.
// A journal makes publication resumable; the workspace identity is written last.
func AdoptStaged(ctx context.Context, r *gitexec.Runner, cwd, stage string, fileKey ...string) (StagedInfo, error) {
	return adoptStaged(ctx, r, cwd, stage, nil, fileKey...)
}

func adoptStaged(ctx context.Context, r *gitexec.Runner, cwd, stage string, fault func(string) error, fileKey ...string) (StagedInfo, error) {
	var empty StagedInfo
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return empty, err
	}
	if w.Admin != w.Common {
		return empty, errors.New("backup restore requires the main checkout")
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return empty, err
	}
	defer common.Close()
	// Restore publishes the private state and the main identity, so it
	// holds the identity lock as git local init does, taken in the same
	// order before the others: git local deinit, which removes the
	// identities that don't belong to the live state, either sees the
	// restored state or runs before it exists.
	identityLocking()
	identities, err := common.Lock(ctx, workspace.IdentityLock, false)
	if err != nil {
		return empty, err
	}
	defer identities.Close()
	lock, err := common.Lock(ctx, RestoreLock, false)
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	if fi, err := common.Lstat(restoreJournalName); err == nil {
		if !fi.Mode().IsRegular() {
			return empty, fmt.Errorf("%w: unsafe backup restore journal", storage.ErrConflict)
		}
		return empty, fmt.Errorf("%w: restore already in progress; run git local backup recover", storage.ErrConflict)
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	for _, name := range []string{"gitlocal", "gitlocal-workspace.json"} {
		if _, err := common.Lstat(name); err == nil {
			return empty, fmt.Errorf("%w: checkout already contains Git Local state", storage.ErrConflict)
		} else if !os.IsNotExist(err) {
			return empty, err
		}
	}
	if filepath.Clean(w.Exclude) != filepath.Join(w.Common, "info", "exclude") {
		return empty, errors.New("backup restore requires the shared Git info/exclude path")
	}
	info, err := ValidateStaged(ctx, r, stage)
	if err != nil {
		return empty, err
	}
	state, err := storage.OpenDir(filepath.Join(stage, "state"))
	if err != nil {
		return empty, err
	}
	defer state.Close()
	var identity workspace.Identity
	root, err := storage.OpenDir(stage)
	if err != nil {
		return empty, err
	}
	if err = root.ReadJSON("identity.json", &identity); err != nil {
		root.Close()
		return empty, err
	}
	root.Close()
	var active []selection.State
	var protected configtx.GuardSnapshot
	for _, id := range info.Workspaces {
		var record workspace.Record
		if err := state.ReadJSON(filepath.Join("workspaces", id, "workspace.json"), &record); err != nil {
			return empty, err
		}
		var selected selection.State
		if err := state.ReadJSON(filepath.Join("workspaces", id, "selection.json"), &selected); err != nil {
			return empty, err
		}
		protected.Selections = append(protected.Selections, configtx.GuardSelection{WorkspaceID: id, State: selected})
		if id == info.WorkspaceID {
			active = append(active, selected)
		}
	}
	ignoreCase, err := r.IgnoreCase(ctx, w.Root, w.Admin)
	if err != nil {
		return empty, err
	}
	patterns, err := exclude.Compile(active, ignoreCase)
	if err != nil {
		return empty, err
	}
	if err := checkMainlineOwnership(ctx, r, *w, protected); err != nil {
		return empty, err
	}
	old, err := common.ReadFile("info/exclude", 1<<20)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return empty, err
	}
	user, err := exclude.UserBytes(old, nil)
	if err != nil {
		return empty, err
	}
	mode := uint32(0600)
	if exists {
		fi, err := common.Lstat("info/exclude")
		if err != nil {
			return empty, err
		}
		mode = uint32(fi.Mode().Perm())
	}
	id, err := storage.UUID()
	if err != nil {
		return empty, err
	}
	prepared := "gitlocal-backup-restore-" + id
	if err := common.MakeDirectory(prepared); err != nil {
		return empty, err
	}
	journaled := false
	defer func() {
		if !journaled {
			os.RemoveAll(filepath.Join(w.Common, prepared))
		}
	}()
	destination, err := storage.OpenDir(filepath.Join(w.Common, prepared))
	if err != nil {
		return empty, err
	}
	if err := copyStage(ctx, state, destination, ""); err != nil {
		destination.Close()
		return empty, err
	}
	if err := destination.MkdirAll("locks"); err != nil {
		destination.Close()
		return empty, err
	}
	if err := destination.StageNamed("locks/repository.lock", 0600, func(io.Writer) error { return nil }); err != nil {
		destination.Close()
		return empty, err
	}
	for _, workspaceID := range info.Workspaces {
		name := filepath.Join("workspaces", workspaceID, "workspace.json")
		var record workspace.Record
		if err := destination.ReadJSON(name, &record); err != nil {
			destination.Close()
			return empty, err
		}
		if workspaceID == info.WorkspaceID {
			record.Root, record.Admin = w.Root, w.Admin
		} else {
			record.State = "retained"
		}
		if err := destination.WriteJSON(name, record); err != nil {
			destination.Close()
			return empty, err
		}
	}
	if err := overlay.ArchiveForAdoption(destination); err != nil {
		destination.Close()
		return empty, err
	}
	// A new clone has neither the old hook files nor the old overlay filter
	// installation. Keep saved overlay versions but drop installation records.
	for _, name := range []string{"hook-install.json", "overlay-install.json", "overlay-filter.gitconfig"} {
		if err := destination.Remove(name); err != nil && !os.IsNotExist(err) {
			destination.Close()
			return empty, err
		}
	}
	if err := prepareSensitiveBackup(ctx, r, *w, filepath.Join(w.Common, prepared), destination, fileKey); err != nil {
		destination.Close()
		return empty, err
	}
	destination.Close()
	j := restoreJournal{SchemaVersion: 1, RepositoryID: info.RepositoryID, WorkspaceID: info.WorkspaceID, Prepared: prepared, Identity: identity, OldExclude: old, NewExclude: exclude.Render(user, patterns), ExcludeExists: exists, ExcludeMode: mode, ArchivedOverlays: info.ArchivedOverlays}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return empty, err
	}
	// It holds two info/exclude images of up to 1 MiB each, base64-encoded;
	// recovery reads it back with the same limit.
	if len(data)+1 > restoreJournalLimit {
		return empty, errors.New("backup restore journal exceeds its size limit")
	}
	if err := common.AtomicWrite(restoreJournalName, append(data, '\n'), 0600); err != nil {
		return empty, err
	}
	journaled = true
	if fault != nil {
		if err := fault("journal"); err != nil {
			return empty, err
		}
	}
	if err := resumeRestore(common, j, fault); err != nil {
		return empty, err
	}
	return info, nil
}

func checkMainlineOwnership(ctx context.Context, r *gitexec.Runner, w workspace.Context, protected configtx.GuardSnapshot) error {
	for _, args := range [][]string{{"ls-files", "-z"}, {"ls-tree", "-r", "--name-only", "-z", w.Head}} {
		if args[0] == "ls-tree" && w.Head == "" {
			continue
		}
		paths, err := r.MainListing(ctx, w.Root, w.Admin, args...)
		if err != nil {
			return err
		}
		for _, p := range bytes.Split(paths, []byte{0}) {
			if len(p) == 0 {
				continue
			}
			conflicts, err := protected.Check([]string{string(p)})
			if err != nil {
				return err
			}
			if len(conflicts) != 0 {
				if conflicts[0].Staging {
					return fmt.Errorf("%w: mainline Git tracks restore or seed staging file %q, which may hold private content; remove it from the index and HEAD first", storage.ErrConflict, p)
				}
				return fmt.Errorf("%w: selected path %q already belongs to mainline Git", storage.ErrConflict, p)
			}
		}
	}
	return nil
}

func copyStage(ctx context.Context, src, dst *storage.Dir, rel string) error {
	entries, err := src.ReadDir(rel)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
			return errors.New("invalid staged backup entry")
		}
		child := filepath.Join(rel, name)
		fi, err := src.Lstat(child)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			if err := dst.MakeDirectory(child); err != nil {
				return err
			}
			if err := copyStage(ctx, src, dst, child); err != nil {
				return err
			}
		} else if fi.Mode().IsRegular() {
			_, err := src.StreamStable(child, func(input io.Reader) error {
				return dst.StageNamed(child, fi.Mode().Perm(), func(output io.Writer) error {
					_, err := io.Copy(output, input)
					return err
				})
			})
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported staged backup entry %q", child)
		}
	}
	return nil
}

// RecoverRestore resumes only a validated restore journal. It never replaces
// an unrelated identity, ignore file, or private state.
func RecoverRestore(ctx context.Context, r *gitexec.Runner, cwd string) (StagedInfo, error) {
	var empty StagedInfo
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return empty, err
	}
	if w.Admin != w.Common {
		return empty, errors.New("backup restore recovery requires the main checkout")
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return empty, err
	}
	defer common.Close()
	// Held for the same reason as in adoptStaged.
	identityLocking()
	identities, err := common.Lock(ctx, workspace.IdentityLock, false)
	if err != nil {
		return empty, err
	}
	defer identities.Close()
	lock, err := common.Lock(ctx, RestoreLock, false)
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	var j restoreJournal
	if err := readRestoreJournal(common, &j); err != nil {
		return empty, err
	}
	if err := validateRestoreJournal(j); err != nil {
		return empty, err
	}
	var identity workspace.Identity
	if err := common.ReadJSON("gitlocal-workspace.json", &identity); os.IsNotExist(err) {
		statePath := j.Prepared
		if _, stateErr := common.Lstat(statePath); os.IsNotExist(stateErr) {
			statePath = "gitlocal"
		} else if stateErr != nil {
			return empty, stateErr
		}
		state, err := storage.OpenDir(filepath.Join(w.Common, statePath))
		if err != nil {
			return empty, err
		}
		entries, err := state.ReadDir("workspaces")
		if err != nil {
			state.Close()
			return empty, err
		}
		var protected configtx.GuardSnapshot
		for _, entry := range entries {
			var selected selection.State
			if err := state.ReadJSON(filepath.Join("workspaces", entry.Name(), "selection.json"), &selected); err != nil {
				state.Close()
				return empty, err
			}
			protected.Selections = append(protected.Selections, configtx.GuardSelection{WorkspaceID: entry.Name(), State: selected})
		}
		state.Close()
		if err := checkMainlineOwnership(ctx, r, *w, protected); err != nil {
			return empty, err
		}
	} else if err != nil {
		return empty, err
	}
	statePath := j.Prepared
	if _, err := common.Lstat(statePath); os.IsNotExist(err) {
		statePath = "gitlocal"
	} else if err != nil {
		return empty, err
	}
	state, err := storage.OpenDir(filepath.Join(w.Common, statePath))
	if err != nil {
		return empty, err
	}
	if err := verifySensitiveBackup(ctx, r, *w, filepath.Join(w.Common, statePath), state); err != nil {
		state.Close()
		return empty, err
	}
	state.Close()
	if err := resumeRestore(common, j, nil); err != nil {
		return empty, err
	}
	return StagedInfo{RepositoryID: j.RepositoryID, WorkspaceID: j.WorkspaceID, ArchivedOverlays: j.ArchivedOverlays}, nil
}

// restoreJournalLimit bounds the backup restore journal when written and
// when read.
const restoreJournalLimit = 8 << 20

// readRestoreJournal reads the backup restore journal strictly, like
// storage.Dir.ReadJSON but with restoreJournalLimit instead of 1 MiB.
func readRestoreJournal(common *storage.Dir, j *restoreJournal) error {
	data, err := common.ReadFile(restoreJournalName, restoreJournalLimit)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(j); err != nil {
		return fmt.Errorf("invalid metadata %s: %w", restoreJournalName, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("invalid trailing metadata in %s", restoreJournalName)
	}
	return nil
}

func validateRestoreJournal(j restoreJournal) error {
	if j.SchemaVersion != 1 || !storage.ValidUUID(j.RepositoryID) || !storage.ValidUUID(j.WorkspaceID) || j.Identity.SchemaVersion != 1 || j.Identity.RepositoryID != j.RepositoryID || j.Identity.WorkspaceID != j.WorkspaceID || !strings.HasPrefix(j.Prepared, "gitlocal-backup-restore-") || !storage.ValidUUID(strings.TrimPrefix(j.Prepared, "gitlocal-backup-restore-")) || len(j.OldExclude) > 1<<20 || len(j.NewExclude) > 1<<20 || j.ExcludeMode > 0777 || j.ArchivedOverlays < 0 || j.ArchivedOverlays > 1000000 {
		return errors.New("invalid backup restore journal")
	}
	return nil
}

func resumeRestore(common *storage.Dir, j restoreJournal, fault func(string) error) error {
	if err := validateRestoreJournal(j); err != nil {
		return err
	}
	var current workspace.Identity
	identityErr := common.ReadJSON("gitlocal-workspace.json", &current)
	if identityErr != nil && !os.IsNotExist(identityErr) {
		return identityErr
	}
	if identityErr == nil && current != j.Identity {
		return fmt.Errorf("%w: workspace identity changed during backup restore", storage.ErrConflict)
	}
	prepared, prepErr := common.Lstat(j.Prepared)
	installed, installErr := common.Lstat("gitlocal")
	if prepErr != nil && !os.IsNotExist(prepErr) || installErr != nil && !os.IsNotExist(installErr) || prepErr == nil && !prepared.IsDir() || installErr == nil && !installed.IsDir() || prepErr == nil && installErr == nil || prepErr != nil && installErr != nil {
		return fmt.Errorf("%w: ambiguous prepared backup restore state", storage.ErrConflict)
	}
	if installErr == nil {
		var repo workspace.Repository
		if err := common.ReadJSON("gitlocal/repository.json", &repo); err != nil {
			return err
		}
		if repo.RepositoryID != j.RepositoryID {
			return fmt.Errorf("%w: restored private state was replaced", storage.ErrConflict)
		}
	}
	before, err := common.ReadFile("info/exclude", 1<<20)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !bytes.Equal(before, j.OldExclude) && !bytes.Equal(before, j.NewExclude) {
		return fmt.Errorf("%w: ignore file changed during backup restore", storage.ErrConflict)
	}
	if !bytes.Equal(before, j.NewExclude) {
		if err := common.AtomicWrite("info/exclude", j.NewExclude, os.FileMode(j.ExcludeMode)); err != nil {
			return err
		}
	}
	if fault != nil {
		if err := fault("exclude"); err != nil {
			return err
		}
	}
	if prepErr == nil {
		if err := common.Rename(j.Prepared, "gitlocal"); err != nil {
			return err
		}
	}
	if fault != nil {
		if err := fault("state"); err != nil {
			return err
		}
	}
	if identityErr != nil {
		if err := common.WriteJSON("gitlocal-workspace.json", j.Identity); err != nil {
			return err
		}
	}
	if fault != nil {
		if err := fault("identity"); err != nil {
			return err
		}
	}
	return common.Remove(restoreJournalName)
}
