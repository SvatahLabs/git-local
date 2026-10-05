package restore

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"runtime"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func validateJournal(ctx context.Context, r *gitexec.Runner, w workspace.Context, state selection.State, digest, id string, j journal) error {
	return validateJournalWithLiveState(ctx, r, w, state, digest, id, j, true)
}

// Doctor can validate an anchored journal from another known workspace, but
// cannot assert that workspace's current selection or working files from here.
func validateJournalWithLiveState(ctx context.Context, r *gitexec.Runner, w workspace.Context, state selection.State, digest, id string, j journal, checkLive bool) error {
	m := j.Manifest
	p := m.Plan
	if (j.SchemaVersion != 1 && j.SchemaVersion != 2) || j.Kind != "restore" || j.ID != id || m.SchemaVersion != j.SchemaVersion || m.Kind != "restore-recovery" || m.ID != id || p.SchemaVersion != 1 || p.RepositoryID != w.RepositoryID || p.DestinationWorkspaceID != w.WorkspaceID || !storage.ValidUUID(p.SourceWorkspaceID) || !validOID(j.RecoverySnapshot) || j.Direction != "apply" && j.Direction != "rollback" || j.Outcome != "" && j.Outcome != "committed" && j.Outcome != "rolledback" && j.Outcome != configtx.OutcomeAbandoned || j.Progress < 0 || j.Progress > len(p.Operations) || (j.Outcome == "committed" || j.Outcome == "rolledback") && j.Progress != len(p.Operations) || len(p.Operations) == 0 || len(m.Post) != len(p.Operations) || len(m.Stages) != len(p.Operations) {
		return errors.New("invalid restore journal identity or schema")
	}
	if j.SchemaVersion == 1 && (p.EncryptedKeyFingerprint != "" || len(m.BeforeStored) != 0 || len(m.PostStored) != 0) || j.SchemaVersion == 2 && (!validFingerprintHex(p.EncryptedKeyFingerprint) || len(m.BeforeStored) != len(p.Operations) || len(m.PostStored) != len(p.Operations)) {
		return errors.New("invalid encrypted restore journal")
	}
	if j.Outcome == "committed" && j.Direction != "apply" || j.Outcome == "rolledback" && j.Direction != "rollback" {
		return errors.New("invalid restore journal outcome")
	}
	if !validOID(p.SourceSnapshot) || !validOID(p.SourceHead) || p.DestinationBaseline != "" && !validOID(p.DestinationBaseline) || len(m.ConfigDigest) != 64 {
		return errors.New("invalid restore evidence")
	}
	canonical, err := selection.Canonical(p.Selection)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(canonical, p.Selection) {
		return errors.New("noncanonical restore selection")
	}
	if len(p.Requests) == 0 || !reflect.DeepEqual(normalizeRequests(append([]string{}, p.Requests...)), p.Requests) {
		return errors.New("noncanonical restore request scope")
	}
	paths := append([]string{}, p.Requests...)
	last := ""
	requiredDirs := map[string]bool{}
	encryptedOperations := 0
	for i, op := range p.Operations {
		if op.Encrypted {
			encryptedOperations++
		}
		if j.SchemaVersion == 1 && (op.Encrypted || op.SourceDigest != "") || j.SchemaVersion == 2 && op.Encrypted && op.Action != "delete" && !validFingerprintHex(op.SourceDigest) || !op.Encrypted && op.SourceDigest != "" {
			return errors.New("invalid encrypted restore operation")
		}
		if op.Path <= last || !p.Selection.Selected(op.Path) || !scoped(op.Path, p.Requests) {
			return errors.New("invalid restore operation scope")
		}
		last = op.Path
		paths = append(paths, op.Path)
		if err = validateFingerprint(op.Before); err != nil {
			return err
		}
		if err = validateFingerprint(m.Post[i]); err != nil {
			return err
		}
		if op.Before.Exists && (op.Before.KeyedDigest != "") != op.Encrypted || m.Post[i].Exists && (m.Post[i].KeyedDigest != "") != op.Encrypted {
			return errors.New("restore image protection differs from operation")
		}
		if !op.Before.Exists && op.Action != "create" || op.Before.Exists && op.Action != "replace" && op.Action != "delete" {
			return errors.New("invalid restore action")
		}
		if op.Action == "delete" {
			if m.Post[i].Exists || op.SourceBlob != "" || op.SourceDigest != "" || op.SourceMode != "" || op.Permissions != 0 {
				return errors.New("invalid deletion postimage")
			}
		} else {
			if !m.Post[i].Exists || !op.Encrypted && op.SourceBlob != m.Post[i].BlobID || op.Encrypted && op.SourceDigest != m.Post[i].KeyedDigest || op.SourceMode != m.Post[i].GitMode || op.Permissions != restorePermissions(op.Before, op.SourceMode) {
				return errors.New("invalid restore postimage")
			}
			permissions := op.Permissions
			if runtime.GOOS == "windows" {
				if permissions&0200 != 0 {
					permissions = 0666
				} else {
					permissions = 0444
				}
			}
			if m.Post[i].Kind == "file" && m.Post[i].Permissions != permissions {
				return errors.New("invalid postimage permissions")
			}
			for dir := path.Dir(op.Path); dir != "."; dir = path.Dir(dir) {
				requiredDirs[dir] = true
			}
		}
		if j.SchemaVersion == 2 {
			if op.Before.Exists && !validOID(m.BeforeStored[i]) || !op.Before.Exists && m.BeforeStored[i] != "" || m.Post[i].Exists && !validOID(m.PostStored[i]) || !m.Post[i].Exists && m.PostStored[i] != "" {
				return errors.New("invalid stored restore blob ID")
			}
			if !op.Encrypted && (op.Before.Exists && m.BeforeStored[i] != op.Before.BlobID || m.Post[i].Exists && m.PostStored[i] != m.Post[i].BlobID) || op.Encrypted && m.Post[i].Exists && m.PostStored[i] != op.SourceBlob {
				return errors.New("restore storage blob and image disagree")
			}
		}
		if imageEqual(op.Before, m.Post[i]) {
			return errors.New("redundant restore operation")
		}
	}
	if j.SchemaVersion == 2 && encryptedOperations == 0 {
		return errors.New("encrypted restore journal has no encrypted operations")
	}
	seenStages := map[string]bool{}
	for i, stages := range m.Stages {
		for _, name := range []string{stages.Apply, stages.Rollback} {
			if path.Dir(name) != path.Dir(p.Operations[i].Path) || !storage.IsTempName(path.Base(name)) || seenStages[name] {
				return errors.New("invalid restore staging path")
			}
			for _, op := range p.Operations {
				if op.Path == name {
					return errors.New("staging collides with operation")
				}
			}
			seenStages[name] = true
			paths = append(paths, name)
		}
	}
	parents := map[string]Parent{}
	last = ""
	for _, parent := range p.Parents {
		if parent.Path <= last || parent.Permissions > 0777 || !parent.Exists && parent.Permissions != 0 {
			return errors.New("invalid restore parent evidence")
		}
		last = parent.Path
		parents[parent.Path] = parent
		paths = append(paths, parent.Path)
	}
	last = ""
	created := map[string]bool{}
	for _, dir := range p.CreateDirectories {
		parent, ok := parents[dir]
		if dir <= last || !requiredDirs[dir] || !ok || parent.Exists {
			return errors.New("invalid restore directory creation")
		}
		last = dir
		created[dir] = true
		paths = append(paths, dir)
	}
	for dir := range requiredDirs {
		parent, ok := parents[dir]
		if !ok || !parent.Exists && !created[dir] {
			return errors.New("missing restore parent evidence")
		}
	}
	if err = validateAliases(paths); err != nil {
		return err
	}
	operationPaths := map[string]bool{}
	for _, op := range p.Operations {
		operationPaths[op.Path] = true
	}
	for _, op := range p.Operations {
		for parent := path.Dir(op.Path); parent != "."; parent = path.Dir(parent) {
			if operationPaths[parent] {
				return errors.New("restore operations have overlapping topology")
			}
		}
	}
	if err = verifyAnchor(ctx, r, w.Store, j); err != nil {
		return err
	}
	// Completed replay is bookkeeping only. Do not reject it because users have
	// legitimately edited files or advanced history after successful completion.
	if j.Outcome != "" {
		return nil
	}
	if checkLive && (digest != m.ConfigDigest || !reflect.DeepEqual(state, p.Selection)) {
		return fmt.Errorf("%w: restore configuration changed", storage.ErrConflict)
	}
	source, err := history.ResolveLocked(ctx, r, w, p.SourceWorkspaceID, p.SourceSnapshot)
	if err != nil {
		return err
	}
	baseline := history.Snapshot{Files: map[string]history.SnapshotFile{}}
	if p.DestinationBaseline != "" {
		baseline, err = history.ResolveLocked(ctx, r, w, w.WorkspaceID, p.DestinationBaseline)
		if err != nil {
			return err
		}
	}
	var key []byte
	if j.SchemaVersion == 2 {
		policy, loaded, e := loadRestoreKey(w)
		if e != nil {
			return e
		}
		if policy.KeyFingerprint != p.EncryptedKeyFingerprint {
			return fmt.Errorf("%w: sensitive-file key differs from restore journal", storage.ErrConflict)
		}
		for _, op := range p.Operations {
			if op.Encrypted != policy.Covers(op.Path) {
				return fmt.Errorf("%w: sensitive-file policy and restore journal disagree for %q", storage.ErrConflict, op.Path)
			}
		}
		key = loaded
	}
	for i, op := range p.Operations {
		src, exists := source.Files[op.Path]
		old, recorded := baseline.Files[op.Path]
		matchesBaseline := matches(op.Before, old, recorded)
		if op.Encrypted {
			matchesBaseline = !op.Before.Exists && !recorded
			if recorded {
				matchesBaseline = op.Before.Exists && op.Before.GitMode == old.Mode && op.Before.KeyedDigest == baseline.Metadata.EncryptedDigests[op.Path]
			}
		}
		if op.Before.Exists && !recorded || op.Dirty != !matchesBaseline || op.Dirty && op.Before.Exists && !p.Overwrite {
			return errors.New("invalid dirty-path authorization")
		}
		if exists != (op.Action != "delete") || exists && (src.ID != op.SourceBlob || src.Mode != op.SourceMode) {
			return errors.New("restore operation disagrees with source snapshot")
		}
		if exists && op.Encrypted {
			if source.Metadata.EncryptedDigests[op.Path] == "" || source.Metadata.EncryptedDigests[op.Path] != op.SourceDigest {
				return errors.New("encrypted restore source is not encrypted")
			}
		}
		for jIndex, im := range []Fingerprint{op.Before, m.Post[i]} {
			if im.Exists {
				stored := im.BlobID
				if j.SchemaVersion == 2 {
					stored = m.BeforeStored[i]
					if jIndex == 1 {
						stored = m.PostStored[i]
					}
				}
				var actual Fingerprint
				var e error
				if op.Encrypted {
					workspaceID := w.WorkspaceID
					if jIndex == 1 {
						workspaceID = p.SourceWorkspaceID
					}
					identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: workspaceID, Path: op.Path, Mode: im.GitMode}
					actual, e = protectedBlobFingerprint(ctx, r, w.Store, stored, key, identity, im.Permissions)
				} else {
					if err = validateSourceMode(ctx, r, w.Store, history.SnapshotFile{Mode: im.GitMode, ID: stored}); err != nil {
						return err
					}
					actual, e = blobFingerprint(ctx, r, w.Store, stored, im.GitMode, im.Permissions)
				}
				if e != nil {
					return e
				}
				if !imageEqual(im, actual) {
					return errors.New("recovery blob disagrees with recorded fingerprint")
				}
			}
		}
	}
	return nil
}
func validateFingerprint(im Fingerprint) error {
	if !im.Exists {
		if im != (Fingerprint{}) {
			return errors.New("invalid absent image")
		}
		return nil
	}
	if im.Size < 0 || im.Permissions > 0777 {
		return errors.New("invalid restore fingerprint")
	}
	if im.KeyedDigest != "" {
		if im.BlobID != "" || im.SHA256 != "" || !validFingerprintHex(im.KeyedDigest) {
			return errors.New("invalid protected restore fingerprint")
		}
	} else if !validOID(im.BlobID) || !validFingerprintHex(im.SHA256) {
		return errors.New("invalid restore fingerprint")
	}
	if im.Kind == "symlink" {
		if im.GitMode != "120000" || im.Size < 1 || im.Size > 1<<20 {
			return errors.New("invalid link preimage")
		}
	} else if im.Kind == "file" {
		mode := "100644"
		if im.Permissions&0111 != 0 {
			mode = "100755"
		}
		if im.GitMode != mode {
			return errors.New("invalid regular preimage mode")
		}
	} else {
		return errors.New("unsupported restore image kind")
	}
	return nil
}

func validFingerprintHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
