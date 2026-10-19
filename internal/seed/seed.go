// Package seed resolves a source snapshot and seeds a freshly initialized
// worktree through one configuration-and-files recovery journal.
package seed

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

func mainWorkspace(ctx context.Context, r *gitexec.Runner, w workspace.Context) (string, error) {
	if bare, err := workspace.Bare(ctx, r, &w); err != nil {
		return "", err
	} else if bare {
		return "", errors.New("this is a worktree of a bare repository, which has no main checkout; use --from <workspace UUID> (see git local workspace list)")
	}
	roots, err := workspace.RegisteredRoots(ctx, r, &w)
	if err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "", errors.New("main worktree is not registered")
	}
	adminPath, err := r.AdminAt(ctx, roots[0])
	if err != nil {
		return "", err
	}
	admin, err := storage.OpenDir(adminPath)
	if err != nil {
		return "", err
	}
	defer admin.Close()
	var identity workspace.Identity
	if err := admin.ReadJSON("gitlocal-workspace.json", &identity); err != nil {
		return "", fmt.Errorf("initialize the primary checkout before using --from main: %w", err)
	}
	if identity.SchemaVersion != workspace.SchemaVersion || identity.RepositoryID != w.RepositoryID || !storage.ValidUUID(identity.WorkspaceID) {
		return "", errors.New("invalid main workspace identity")
	}
	return identity.WorkspaceID, nil
}

func sourceRule(snapshot history.Snapshot, p string) (selection.Rule, error) {
	if !snapshot.Metadata.Selection.Selected(p) {
		return selection.Rule{}, fmt.Errorf("%w: source does not select %q", storage.ErrConflict, p)
	}
	if _, ok := snapshot.Files[p]; ok {
		return selection.Rule{Path: p, Kind: "file"}, nil
	}
	if snapshot.Directories[p] {
		return selection.Rule{Path: p, Kind: "directory"}, nil
	}
	for _, rule := range snapshot.Metadata.Selection.Include {
		if rule.Path == p {
			return rule, nil
		}
	}
	return selection.Rule{}, fmt.Errorf("%w: source has no recorded type for %q", storage.ErrConflict, p)
}

func Run(ctx context.Context, r *gitexec.Runner, cwd, from string, paths []string) (configtx.SeedResult, error) {
	if from != "main" && !storage.ValidUUID(from) {
		return configtx.SeedResult{}, errors.New("--from requires main or a workspace UUID")
	}
	initialized, err := workspace.Init(ctx, r, cwd)
	if err != nil {
		return configtx.SeedResult{}, err
	}
	result, err := configtx.Seed(ctx, r, cwd, func(w workspace.Context) (configtx.SeedPlan, error) {
		result := configtx.SeedPlan{}
		id := from
		var err error
		if id == "main" {
			id, err = mainWorkspace(ctx, r, w)
			if err != nil {
				return result, err
			}
		}
		if id == w.WorkspaceID {
			return result, errors.New("seed source and destination are the same workspace")
		}
		baseline, err := history.WorkspaceHeadLocked(ctx, r, w, w.WorkspaceID)
		if err != nil {
			return result, err
		}
		if baseline != "" {
			return result, fmt.Errorf("%w: destination already has local snapshots", storage.ErrConflict)
		}
		snapshot, err := history.ResolveLocked(ctx, r, w, id, "latest")
		if err != nil {
			return result, err
		}
		engine, err := selection.New(r, w.Root, w.Admin)
		if err != nil {
			return result, err
		}
		defer engine.Close()
		var state selection.State
		if len(paths) == 0 {
			state = snapshot.Metadata.Selection
			state.Generation = 1
		} else {
			rules := []selection.Rule{}
			for _, arg := range paths {
				p, e := engine.Normalize(cwd, arg)
				if e != nil {
					return result, e
				}
				rule, e := sourceRule(snapshot, p)
				if e != nil {
					return result, e
				}
				rules = append(rules, rule)
			}
			plan, e := engine.PlanHistorical(ctx, selection.Empty(), rules)
			if e != nil {
				return result, e
			}
			state = plan.State
			// Preserve exclusions within copied directory rules.
			for _, excluded := range snapshot.Metadata.Selection.Exclude {
				for _, rule := range state.Include {
					if rule.Kind == "directory" && (excluded == rule.Path || len(excluded) > len(rule.Path) && excluded[:len(rule.Path)+1] == rule.Path+"/") {
						state.Exclude = append(state.Exclude, excluded)
						break
					}
				}
			}
		}
		state, err = selection.Canonical(state)
		if err != nil {
			return result, err
		}
		result.SourceWorkspace, result.SourceSnapshot, result.Selection = id, snapshot.ID, state
		var encrypted []string
		for p, file := range snapshot.Files {
			if state.Selected(p) {
				seedFile := configtx.SeedFile{Path: p, Mode: file.Mode, Blob: file.ID}
				if snapshot.Metadata.EncryptedDigests[p] != "" {
					encrypted = append(encrypted, p)
					seedFile.PlainDigest = snapshot.Metadata.EncryptedDigests[p]
					result.KeyFingerprint = snapshot.Metadata.EncryptedKeyFingerprint
				}
				result.Files = append(result.Files, seedFile)
			}
		}
		sort.Strings(encrypted)
		if err := history.VerifyEncryptedFilesLocked(ctx, r, w, snapshot, encrypted); err != nil {
			return result, err
		}
		sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
		return result, nil
	})
	result.Warnings = append(initialized.Warnings, result.Warnings...)
	return result, err
}

func Recover(ctx context.Context, r *gitexec.Runner, cwd, id string, rollback bool) (configtx.SeedResult, error) {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return configtx.SeedResult{}, err
	}
	validate := func(w workspace.Context, sourceID, snapshotID string, state selection.State, files []configtx.SeedFile) error {
		if rollback {
			return nil // rollback only removes journal-owned destination files
		}
		snapshot, err := history.ResolveLocked(ctx, r, w, sourceID, snapshotID)
		if err != nil {
			return err
		}
		if snapshot.ID != snapshotID {
			return errors.New("seed source snapshot changed")
		}
		for _, rule := range state.Include {
			original, err := sourceRule(snapshot, rule.Path)
			if err != nil || original.Kind != rule.Kind {
				return fmt.Errorf("%w: seed selection differs from source snapshot", storage.ErrConflict)
			}
		}
		excludes := map[string]bool{}
		for _, p := range snapshot.Metadata.Selection.Exclude {
			excludes[p] = true
		}
		for _, p := range state.Exclude {
			if !excludes[p] {
				return fmt.Errorf("%w: seed exclusion differs from source snapshot", storage.ErrConflict)
			}
		}
		expected := map[string]history.SnapshotFile{}
		for p, file := range snapshot.Files {
			if state.Selected(p) {
				expected[p] = file
			}
		}
		if len(expected) != len(files) {
			return errors.New("seed files differ from source snapshot")
		}
		var encrypted []string
		for _, file := range files {
			original, ok := expected[file.Path]
			if !ok || original.Mode != file.Mode || original.ID != file.Blob {
				return errors.New("seed file differs from source snapshot")
			}
			if snapshot.Metadata.EncryptedDigests[file.Path] != "" {
				if snapshot.Metadata.EncryptedDigests[file.Path] != file.PlainDigest {
					return fmt.Errorf("%w: encrypted seed file differs from source snapshot", storage.ErrConflict)
				}
				encrypted = append(encrypted, file.Path)
			} else if file.PlainDigest != "" {
				return errors.New("unexpected sensitive seed digest")
			}
		}
		if err := history.VerifyEncryptedFilesLocked(ctx, r, w, snapshot, encrypted); err != nil {
			return fmt.Errorf("%w: encrypted seed file differs from source snapshot", storage.ErrConflict)
		}
		return nil
	}
	return configtx.RecoverSeed(ctx, r, cwd, id, rollback, validate)
}
