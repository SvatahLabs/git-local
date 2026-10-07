package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// OrphanRef is a recovery ref that no restore journal names. Quarantine is
// the quarantine directory holding its journal when an abandoned encryption
// migration set the journal aside; the ref then still keeps that restore's
// recovery snapshot.
type OrphanRef struct {
	Ref, WorkspaceID, ID, Commit string
	Quarantine                   string
}

// OrphanRefs lists the recovery refs of restores that stopped after
// publishing their anchor but before writing their journal, for example
// because the process was killed. No working file changes before the journal
// exists, so such a ref holds nothing to recover; it only blocks encryption
// migration as an unknown private ref. Only refs whose commit is this
// restore's recovery commit are listed.
func OrphanRefs(ctx context.Context, r *gitexec.Runner, w workspace.Context) ([]OrphanRef, error) {
	meta, err := openMeta(w.Common)
	if err != nil {
		return nil, err
	}
	defer meta.Close()
	b, err := r.History(ctx, w.Store, nil, "show-ref")
	var ge *gitexec.Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return nil, nil // No refs at all.
	}
	if err != nil {
		return nil, err
	}
	var orphans []OrphanRef
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		commit, ref, ok := strings.Cut(line, " ")
		rest, recovery := strings.CutPrefix(ref, "refs/gitlocal/recovery/")
		if !ok || !recovery {
			continue
		}
		workspaceID, id, ok := strings.Cut(rest, "/")
		if !ok || !storage.ValidUUID(workspaceID) || !storage.ValidUUID(id) {
			continue
		}
		journaled := false
		for _, name := range []string{journalPath(id), completedPath(id)} {
			if _, err := meta.Lstat(name); err == nil {
				journaled = true
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
		if journaled {
			continue
		}
		object, err := r.History(ctx, w.Store, nil, "cat-file", "commit", commit)
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(string(object), "\n\nGit Local restore recovery "+id+"\n") {
			continue
		}
		orphan := OrphanRef{Ref: ref, WorkspaceID: workspaceID, ID: id, Commit: commit}
		quarantines, err := meta.ReadDir(configtx.QuarantineDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, q := range quarantines {
			if _, err := meta.Lstat(filepath.Join(configtx.QuarantineDir, q.Name(), "journals", filepath.Base(completedPath(id)))); err == nil {
				orphan.Quarantine = filepath.Join(w.Common, "gitlocal", configtx.QuarantineDir, q.Name())
			}
		}
		orphans = append(orphans, orphan)
	}
	return orphans, nil
}

// AbandonOrphanRef deletes the recovery refs of restore id, which never wrote
// its journal. It returns an error matching os.ErrNotExist when there is no
// such ref.
func AbandonOrphanRef(ctx context.Context, r *gitexec.Runner, cwd, id string) (configtx.AbandonResult, error) {
	result := configtx.AbandonResult{TransactionID: id, Kind: "recovery-ref"}
	if !storage.ValidUUID(id) {
		return result, errors.New("invalid transaction ID")
	}
	err := configtx.WithRestore(ctx, r, cwd, id, func(w workspace.Context, _ selection.State, _ string, _ func() error) error {
		orphans, err := OrphanRefs(ctx, r, w)
		if err != nil {
			return err
		}
		for _, orphan := range orphans {
			if orphan.ID != id {
				continue
			}
			if _, err := r.History(ctx, w.Store, nil, "update-ref", "--no-deref", "-d", orphan.Ref, orphan.Commit); err != nil {
				return err
			}
			result.Removed = append(result.Removed, orphan.Ref)
			if orphan.Quarantine != "" {
				result.Warnings = append(result.Warnings, "the restore's journal is quarantined in "+orphan.Quarantine+"; its recovery snapshot "+orphan.Commit+" is no longer referenced")
			}
		}
		if len(result.Removed) == 0 {
			return os.ErrNotExist
		}
		return nil
	})
	return result, err
}
