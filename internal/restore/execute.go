package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"runtime"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// Execute always builds a fresh plan under the exclusive repository lock. Public
// dry-run JSON is never accepted as execution input.
func Execute(ctx context.Context, r *gitexec.Runner, cwd, sourceWorkspace, selector string, paths []string, overwrite bool) (Result, error) {
	return execute(ctx, r, cwd, sourceWorkspace, selector, paths, overwrite, nil)
}
func execute(ctx context.Context, r *gitexec.Runner, cwd, sourceWorkspace, selector string, paths []string, overwrite bool, f fault) (Result, error) {
	var result Result
	if err := history.ValidateSelector(selector); err != nil {
		return result, err
	}
	if len(paths) == 0 {
		return result, errors.New("restore requires explicit paths")
	}
	err := configtx.WithRestore(ctx, r, cwd, "", func(w workspace.Context, state selection.State, digest string, recheck func() error) error {
		p, err := planLocked(ctx, r, cwd, w, state, recheck, sourceWorkspace, selector, paths, overwrite, nil)
		if err != nil {
			return err
		}
		if len(p.data.Operations) == 0 {
			return nil
		}
		var key []byte
		if p.data.EncryptedKeyFingerprint != "" {
			policy, loaded, e := loadRestoreKey(w)
			if e != nil {
				return e
			}
			if policy.KeyFingerprint != p.data.EncryptedKeyFingerprint {
				return fmt.Errorf("%w: sensitive-file key changed during restore planning", storage.ErrConflict)
			}
			for _, op := range p.data.Operations {
				if op.Encrypted != policy.Covers(op.Path) {
					return fmt.Errorf("%w: sensitive-file policy and restore plan disagree for %q", storage.ErrConflict, op.Path)
				}
			}
			key = loaded
		}
		id, err := storage.UUID()
		if err != nil {
			return err
		}
		result.TransactionID = id
		branch, err := mainBranch(ctx, r, w)
		if err != nil {
			return err
		}
		version := 1
		if p.data.EncryptedKeyFingerprint != "" {
			version = 2
		}
		m := manifest{SchemaVersion: version, Kind: "restore-recovery", ID: id, Plan: p.data, ConfigDigest: digest, MainBranch: branch, Post: []Fingerprint{}}
		files, err := storage.OpenDir(w.Root)
		if err != nil {
			return err
		}
		defer files.Close()
		for _, op := range p.data.Operations {
			paths := stagePaths{}
			for _, target := range []*string{&paths.Apply, &paths.Rollback} {
				token, e := storage.UUID()
				if e != nil {
					return e
				}
				*target = path.Join(path.Dir(op.Path), ".gitlocal-tmp-"+token)
				if _, e = files.Lstat(*target); e == nil {
					return errors.New("staging path already exists")
				} else if !os.IsNotExist(e) {
					return e
				}
			}
			m.Stages = append(m.Stages, paths)
		}
		for i, op := range p.data.Operations {
			if runtime.GOOS == "windows" && (op.Before.Kind == "symlink" || op.Before.GitMode == "100755") {
				return errors.New("preimage mode cannot be safely rolled back on Windows")
			}
			if err = point(ctx, f, fmt.Sprintf("before-capture:%d", i)); err != nil {
				return err
			}
			storedBefore := ""
			if op.Before.Exists {
				var blob string
				writeCaptured := func(input io.Reader) error {
					var e error
					if op.Encrypted {
						identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Path: op.Path, Mode: op.Before.GitMode}
						blob, e = secureblob.EncryptGitBlob(ctx, r, w.Store, input, key, identity)
					} else {
						blob, e = hashBlob(ctx, r, w.Store, input)
					}
					return e
				}
				if op.Before.Kind == "symlink" {
					target, e := files.ReadLink(op.Path)
					if e != nil {
						return e
					}
					err = writeCaptured(strings.NewReader(target))
				} else {
					_, err = files.StreamStable(op.Path, writeCaptured)
				}
				if err != nil {
					return err
				}
				storedBefore = blob
				var captured Fingerprint
				var e error
				if op.Encrypted {
					identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Path: op.Path, Mode: op.Before.GitMode}
					captured, e = protectedBlobFingerprint(ctx, r, w.Store, blob, key, identity, op.Before.Permissions)
				} else {
					captured, e = blobFingerprint(ctx, r, w.Store, blob, op.Before.GitMode, op.Before.Permissions)
				}
				if e != nil {
					return e
				}
				if !imageEqual(captured, op.Before) {
					return errors.New("recovery capture disagrees with planned preimage")
				}
			}
			if version == 2 {
				m.BeforeStored = append(m.BeforeStored, storedBefore)
			}
			if err = point(ctx, f, fmt.Sprintf("capture:%d", i)); err != nil {
				return err
			}
			post := Fingerprint{}
			storedPost := ""
			if op.SourceBlob != "" {
				storedPost = op.SourceBlob
				permissions := op.Permissions
				if runtime.GOOS == "windows" {
					if permissions&0200 != 0 {
						permissions = 0666
					} else {
						permissions = 0444
					}
				}
				if op.Encrypted {
					identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: p.data.SourceWorkspaceID, Path: op.Path, Mode: op.SourceMode}
					post, err = protectedBlobFingerprint(ctx, r, w.Store, op.SourceBlob, key, identity, permissions)
					if err == nil && post.KeyedDigest != op.SourceDigest {
						err = fmt.Errorf("%w: encrypted source changed during recovery capture", storage.ErrConflict)
					}
				} else {
					post, err = blobFingerprint(ctx, r, w.Store, op.SourceBlob, op.SourceMode, permissions)
				}
				if err != nil {
					return err
				}
			}
			m.Post = append(m.Post, post)
			if version == 2 {
				m.PostStored = append(m.PostStored, storedPost)
			}
		}
		j := journal{SchemaVersion: version, Kind: "restore", ID: id, Manifest: m, Direction: "apply"}
		guard, err := newGuard(ctx, r, w, files, &j, recheck)
		if err != nil {
			return err
		}
		defer guard.close()
		if err = guard.recognize(ctx); err != nil {
			return err
		}
		j.RecoverySnapshot, err = anchor(ctx, r, w.Store, m, f)
		if err != nil {
			return err
		}
		result.RecoverySnapshot = j.RecoverySnapshot
		if err = guard.recognize(ctx); err != nil {
			return err
		}
		meta, err := openMeta(w.Common)
		if err != nil {
			return err
		}
		defer meta.Close()
		if err = point(ctx, f, "before-journal"); err != nil {
			return err
		}
		if err = meta.MkdirAll("transactions"); err != nil {
			return err
		}
		if err = writeJournal(meta, j); err != nil {
			return stopped(id, err)
		}
		if err = point(ctx, f, "journal"); err != nil {
			return stopped(id, err)
		}
		if err = run(ctx, r, w, files, meta, &j, guard, f); err != nil {
			return stopped(id, err)
		}
		result.Changed = true
		return nil
	})
	return result, err
}

func mainBranch(ctx context.Context, r *gitexec.Runner, w workspace.Context) (string, error) {
	b, err := r.Main(ctx, w.Root, w.Admin, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		var ge *gitexec.Error
		if errors.As(err, &ge) && ge.Code == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
func imageEqual(a, b Fingerprint) bool {
	if a.Kind == "symlink" {
		a.Permissions = 0
	}
	if b.Kind == "symlink" {
		b.Permissions = 0
	}
	return a == b
}

type guard struct {
	r       *gitexec.Runner
	w       workspace.Context
	files   *storage.Dir
	engine  *selection.Engine
	journal *journal
	recheck func() error
	key     []byte
}

func newGuard(ctx context.Context, r *gitexec.Runner, w workspace.Context, files *storage.Dir, j *journal, recheck func() error) (*guard, error) {
	e, err := selection.New(r, w.Root, w.Admin)
	if err != nil {
		return nil, err
	}
	g := &guard{r: r, w: w, files: files, engine: e, journal: j, recheck: recheck}
	if j.SchemaVersion == 2 {
		policy, key, e := loadRestoreKey(w)
		if e != nil {
			g.close()
			return nil, e
		}
		if policy.KeyFingerprint != j.Manifest.Plan.EncryptedKeyFingerprint {
			g.close()
			return nil, fmt.Errorf("%w: sensitive-file key changed during restore", storage.ErrConflict)
		}
		for _, op := range j.Manifest.Plan.Operations {
			if op.Encrypted != policy.Covers(op.Path) {
				g.close()
				return nil, fmt.Errorf("%w: sensitive-file policy and restore journal disagree for %q", storage.ErrConflict, op.Path)
			}
		}
		g.key = key
	}
	owner, err := selection.InspectOwnership(ctx, r, w.Root, w.Admin)
	if err != nil {
		e.Close()
		return nil, err
	}
	if owner.Evidence != j.Manifest.Plan.Ownership {
		e.Close()
		return nil, fmt.Errorf("%w: main ownership changed", storage.ErrConflict)
	}
	for _, op := range j.Manifest.Plan.Operations {
		s := selection.Empty()
		s.Include = []selection.Rule{{Path: op.Path, Kind: "directory"}}
		if err = owner.Check(s); err != nil {
			e.Close()
			return nil, err
		}
	}
	for _, stages := range j.Manifest.Stages {
		for _, name := range []string{stages.Apply, stages.Rollback} {
			scope := selection.Empty()
			scope.Include = []selection.Rule{{Path: name, Kind: "directory"}}
			if err = owner.Check(scope); err != nil {
				e.Close()
				return nil, err
			}
		}
	}
	return g, nil
}
func (g *guard) close() { g.engine.Close() }
func (g *guard) matchesImage(ctx context.Context, path string, image Fingerprint, workspaceID, identityPath string) (bool, error) {
	var actual Fingerprint
	var err error
	if image.KeyedDigest != "" {
		identity := secureblob.Identity{RepositoryID: g.w.RepositoryID, WorkspaceID: workspaceID, Path: identityPath, Mode: image.GitMode}
		protected, e := inspectSensitiveFile(g.files, path, g.key, identity)
		err = e
		actual = protectedFingerprint(protected)
	} else {
		actual, err = inspectFile(ctx, g.r, g.w.Store, g.files, path)
	}
	return imageEqual(actual, image), err
}
func (g *guard) recognize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.recheck(); err != nil {
		return err
	}
	m := g.journal.Manifest
	p := m.Plan
	if err := (selection.Ownership{Evidence: p.Ownership}).Revalidate(ctx, g.r, g.w.Root, g.w.Admin); err != nil {
		return err
	}
	branch, err := mainBranch(ctx, g.r, g.w)
	if err != nil {
		return err
	}
	if branch != m.MainBranch {
		return fmt.Errorf("%w: main branch changed", storage.ErrConflict)
	}
	for _, entry := range []struct{ id, head string }{{p.SourceWorkspaceID, p.SourceHead}, {p.DestinationWorkspaceID, p.DestinationBaseline}} {
		head, e := history.WorkspaceHeadLocked(ctx, g.r, g.w, entry.id)
		if e != nil {
			return e
		}
		if head != entry.head {
			return fmt.Errorf("%w: user snapshot head changed", storage.ErrConflict)
		}
	}
	for i, op := range p.Operations {
		if err = g.engine.CheckRestorePath(op.Path); err != nil {
			return err
		}
		beforeMatch, e := g.matchesImage(ctx, op.Path, op.Before, g.w.WorkspaceID, op.Path)
		if e != nil {
			return e
		}
		postMatch, e := g.matchesImage(ctx, op.Path, m.Post[i], p.SourceWorkspaceID, op.Path)
		if e != nil {
			return e
		}
		if !beforeMatch && !postMatch {
			return fmt.Errorf("%w: new external edit at %q; preserved", storage.ErrConflict, op.Path)
		}
	}
	for i, stages := range m.Stages {
		for _, entry := range []struct {
			path  string
			image Fingerprint
		}{{stages.Apply, m.Post[i]}, {stages.Rollback, p.Operations[i].Before}} {
			if err = g.engine.CheckRestorePath(entry.path); err != nil {
				return err
			}
			workspaceID := g.w.WorkspaceID
			if entry.path == stages.Apply {
				workspaceID = p.SourceWorkspaceID
			}
			match, e := g.matchesImage(ctx, entry.path, entry.image, workspaceID, p.Operations[i].Path)
			if e != nil {
				return e
			}
			info, e := g.files.Lstat(entry.path)
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			if info != nil && !match {
				return fmt.Errorf("%w: unrecognized staging bytes at %q; preserved", storage.ErrConflict, entry.path)
			}
		}
	}
	created := map[string]bool{}
	for _, p := range p.CreateDirectories {
		created[p] = true
	}
	for _, parent := range p.Parents {
		now, e := inspectParent(g.files, parent.Path)
		if e != nil {
			return e
		}
		if parent == now {
			continue
		}
		permissions := uint32(0700)
		if runtime.GOOS == "windows" {
			permissions = 0777
		}
		if !parent.Exists && created[parent.Path] && now.Exists && now.Permissions == permissions {
			continue
		}
		return fmt.Errorf("%w: parent directory changed at %q", storage.ErrConflict, parent.Path)
	}
	return nil
}
func run(ctx context.Context, r *gitexec.Runner, w workspace.Context, files, meta *storage.Dir, j *journal, g *guard, f fault) error {
	if j.Outcome != "" {
		return archive(ctx, meta, *j, f)
	}
	var key []byte
	if j.SchemaVersion == 2 {
		policy, loaded, err := loadRestoreKey(w)
		if err != nil {
			return err
		}
		if policy.KeyFingerprint != j.Manifest.Plan.EncryptedKeyFingerprint {
			return fmt.Errorf("%w: sensitive-file key changed during restore", storage.ErrConflict)
		}
		for _, op := range j.Manifest.Plan.Operations {
			if op.Encrypted != policy.Covers(op.Path) {
				return fmt.Errorf("%w: sensitive-file policy and restore journal disagree for %q", storage.ErrConflict, op.Path)
			}
		}
		key = loaded
	}
	rollback := j.Direction == "rollback"
	if !rollback {
		for i, dir := range j.Manifest.Plan.CreateDirectories {
			if err := point(ctx, f, fmt.Sprintf("before-directory:%d", i)); err != nil {
				return err
			}
			if err := g.recognize(ctx); err != nil {
				return err
			}
			before, err := inspectParent(files, dir)
			if err != nil {
				return err
			}
			if !before.Exists {
				if err = files.MakeDirectory(dir); err != nil {
					return err
				}
			}
			if err = point(ctx, f, fmt.Sprintf("directory:%d", i)); err != nil {
				return err
			}
		}
	}
	for i, op := range j.Manifest.Plan.Operations {
		desired := j.Manifest.Post[i]
		if rollback {
			desired = op.Before
		}
		if err := g.recognize(ctx); err != nil {
			return err
		}
		workspaceID := j.Manifest.Plan.SourceWorkspaceID
		if rollback {
			workspaceID = w.WorkspaceID
		}
		match, err := g.matchesImage(ctx, op.Path, desired, workspaceID, op.Path)
		if err != nil {
			return err
		}
		if !match {
			if err = install(ctx, r, w, files, g, op.Path, desired, key, i, f); err != nil {
				return err
			}
		}
		if err = point(ctx, f, fmt.Sprintf("before-progress:%d", i)); err != nil {
			return err
		}
		j.Progress = i + 1
		if err = writeJournal(meta, *j); err != nil {
			return err
		}
		if err = point(ctx, f, fmt.Sprintf("progress:%d", i)); err != nil {
			return err
		}
	}
	if err := cleanupStages(ctx, r, w, files, g, f); err != nil {
		return err
	}
	if rollback {
		for i := len(j.Manifest.Plan.CreateDirectories) - 1; i >= 0; i-- {
			dir := j.Manifest.Plan.CreateDirectories[i]
			if err := point(ctx, f, fmt.Sprintf("before-remove-directory:%d", i)); err != nil {
				return err
			}
			if err := g.recognize(ctx); err != nil {
				return err
			}
			current, err := inspectParent(files, dir)
			if err != nil {
				return err
			}
			if current.Exists {
				if err = files.RemoveDirectory(dir); err != nil {
					return fmt.Errorf("%w: cannot remove created directory %q (unknown contents preserved): %v", storage.ErrConflict, dir, err)
				}
			}
			if err = point(ctx, f, fmt.Sprintf("remove-directory:%d", i)); err != nil {
				return err
			}
		}
	}
	if err := g.recognize(ctx); err != nil {
		return err
	}
	for i, op := range j.Manifest.Plan.Operations {
		want := j.Manifest.Post[i]
		if rollback {
			want = op.Before
		}
		workspaceID := j.Manifest.Plan.SourceWorkspaceID
		if rollback {
			workspaceID = w.WorkspaceID
		}
		match, err := g.matchesImage(ctx, op.Path, want, workspaceID, op.Path)
		if err != nil {
			return err
		}
		if !match {
			return fmt.Errorf("%w: final restore image changed", storage.ErrConflict)
		}
	}
	if err := point(ctx, f, "before-outcome"); err != nil {
		return err
	}
	j.Outcome = "committed"
	if rollback {
		j.Outcome = "rolledback"
	}
	if err := writeJournal(meta, *j); err != nil {
		return err
	}
	if err := point(ctx, f, "outcome"); err != nil {
		return err
	}
	return archive(ctx, meta, *j, f)
}
func install(ctx context.Context, r *gitexec.Runner, w workspace.Context, files *storage.Dir, g *guard, path string, desired Fingerprint, key []byte, i int, f fault) error {
	var stage string
	if desired.Exists {
		op := g.journal.Manifest.Plan.Operations[i]
		stored := desired.BlobID
		identity := secureblob.Identity{RepositoryID: w.RepositoryID, WorkspaceID: g.journal.Manifest.Plan.SourceWorkspaceID, Path: path, Mode: desired.GitMode}
		if g.journal.SchemaVersion == 2 {
			stored = g.journal.Manifest.PostStored[i]
			if g.journal.Direction == "rollback" {
				stored = g.journal.Manifest.BeforeStored[i]
				identity.WorkspaceID = w.WorkspaceID
			}
		}
		stage = g.journal.Manifest.Stages[i].Apply
		if g.journal.Direction == "rollback" {
			stage = g.journal.Manifest.Stages[i].Rollback
		}
		if err := point(ctx, f, fmt.Sprintf("before-stage:%d", i)); err != nil {
			return err
		}
		actual, err := inspectFile(ctx, r, w.Store, files, stage)
		if err != nil {
			return err
		}
		if actual.Exists {
			match, e := g.matchesImage(ctx, stage, desired, identity.WorkspaceID, path)
			if e != nil {
				return e
			}
			if !match {
				return fmt.Errorf("%w: unrecognized staging bytes at %q; preserved", storage.ErrConflict, stage)
			}
		} else {
			if desired.Kind == "symlink" {
				if op.Encrypted {
					var target bytes.Buffer
					validator := &symlinkContentValidator{}
					err = secureblob.DecryptGitBlob(ctx, r, w.Store, stored, io.MultiWriter(validator, &target), key, identity)
					if err == nil && target.Len() == 0 {
						err = errors.New("empty encrypted symlink target")
					}
					if err == nil {
						err = files.StageNamedSymlink(stage, target.String())
					}
				} else {
					b, e := r.History(ctx, w.Store, nil, "cat-file", "blob", stored)
					if e != nil {
						return e
					}
					err = files.StageNamedSymlink(stage, string(b))
				}
			} else {
				err = files.StageNamed(stage, os.FileMode(desired.Permissions), func(out io.Writer) error {
					writer := stageWriter{ctx: ctx, out: out, f: f, index: i}
					if op.Encrypted {
						return secureblob.DecryptGitBlob(ctx, r, w.Store, stored, writer, key, identity)
					}
					return r.HistoryBlob(ctx, w.Store, stored, writer)
				})
			}
			if err != nil {
				return err
			}
		}
		if err = point(ctx, f, fmt.Sprintf("stage:%d", i)); err != nil {
			return err
		}
	}

	if desired.Exists {
		if err := files.SyncStaged(stage); err != nil {
			return err
		}
	}
	if err := point(ctx, f, fmt.Sprintf("before-install:%d", i)); err != nil {
		return err
	}
	if err := g.recognize(ctx); err != nil {
		return err
	}
	// Check staging again after the fault boundary and immediately before rename.
	if desired.Exists {
		workspaceID := g.journal.Manifest.Plan.SourceWorkspaceID
		if g.journal.Direction == "rollback" {
			workspaceID = w.WorkspaceID
		}
		match, err := g.matchesImage(ctx, stage, desired, workspaceID, path)
		if err != nil {
			return err
		}
		if !match {
			return fmt.Errorf("%w: staging file changed", storage.ErrConflict)
		}
	}
	var err error
	if desired.Exists {
		err = files.Rename(stage, path)
	} else {
		err = files.Remove(path)
	}
	if err != nil {
		return err
	}
	return point(ctx, f, fmt.Sprintf("install:%d", i))
}

// Recover requires the original destination checkout, never an adopted identity.
func Recover(ctx context.Context, r *gitexec.Runner, cwd, id string, rollback bool) (Result, error) {
	return recoverRestore(ctx, r, cwd, id, rollback, nil)
}
func recoverRestore(ctx context.Context, r *gitexec.Runner, cwd, id string, rollback bool, f fault) (Result, error) {
	var result Result
	if !storage.ValidUUID(id) {
		return result, errors.New("invalid restore transaction ID")
	}
	err := configtx.WithRestore(ctx, r, cwd, id, func(w workspace.Context, state selection.State, digest string, recheck func() error) error {
		meta, err := openMeta(w.Common)
		if err != nil {
			return err
		}
		defer meta.Close()
		j, err := readJournal(meta, journalPath(id))
		completed := false
		if os.IsNotExist(err) {
			j, err = readJournal(meta, completedPath(id))
			completed = true
		}
		if err != nil {
			return err
		}
		if err = validateJournal(ctx, r, w, state, digest, id, j); err != nil {
			return err
		}
		result = Result{TransactionID: id, RecoverySnapshot: j.RecoverySnapshot}
		if completed || j.Outcome != "" {
			if rollback && j.Outcome != "rolledback" || !rollback && j.Outcome != "committed" {
				return errors.New("restore already completed with a different outcome")
			}
			if completed {
				return nil
			}
			return archive(ctx, meta, j, f)
		}
		if j.Direction == "rollback" && !rollback {
			return errors.New("rollback has started; use --rollback to finish it")
		}
		if digest != j.Manifest.ConfigDigest || !reflect.DeepEqual(state, j.Manifest.Plan.Selection) {
			return fmt.Errorf("%w: configuration changed since restore began", storage.ErrConflict)
		}
		files, err := storage.OpenDir(w.Root)
		if err != nil {
			return err
		}
		defer files.Close()
		g, err := newGuard(ctx, r, w, files, &j, recheck)
		if err != nil {
			return stopped(id, err)
		}
		defer g.close()
		if err = g.recognize(ctx); err != nil {
			return stopped(id, err)
		}
		if rollback {
			if err = point(ctx, f, "before-rollback-intent"); err != nil {
				return stopped(id, err)
			}
			j.Direction = "rollback"
			j.Progress = 0
			if err = writeJournal(meta, j); err != nil {
				return stopped(id, err)
			}
			if err = point(ctx, f, "rollback-intent"); err != nil {
				return stopped(id, err)
			}
		}
		if err = run(ctx, r, w, files, meta, &j, g, f); err != nil {
			return stopped(id, err)
		}
		result.Changed = true
		return nil
	})
	return result, err
}

func cleanupStages(ctx context.Context, r *gitexec.Runner, w workspace.Context, files *storage.Dir, g *guard, f fault) error {
	for i, stages := range g.journal.Manifest.Stages {
		for _, entry := range []struct {
			path  string
			image Fingerprint
		}{{stages.Apply, g.journal.Manifest.Post[i]}, {stages.Rollback, g.journal.Manifest.Plan.Operations[i].Before}} {
			now, err := inspectFile(ctx, r, w.Store, files, entry.path)
			if err != nil {
				return err
			}
			if !now.Exists {
				continue
			}
			workspaceID := w.WorkspaceID
			if entry.path == stages.Apply {
				workspaceID = g.journal.Manifest.Plan.SourceWorkspaceID
			}
			match, err := g.matchesImage(ctx, entry.path, entry.image, workspaceID, g.journal.Manifest.Plan.Operations[i].Path)
			if err != nil {
				return err
			}
			if !match {
				return fmt.Errorf("%w: unrecognized staging bytes at %q; preserved", storage.ErrConflict, entry.path)
			}
			if err = point(ctx, f, fmt.Sprintf("before-stage-cleanup:%d", i)); err != nil {
				return err
			}
			if err = g.recognize(ctx); err != nil {
				return err
			}
			match, e := g.matchesImage(ctx, entry.path, entry.image, workspaceID, g.journal.Manifest.Plan.Operations[i].Path)
			if e != nil {
				return e
			}
			if !match {
				return fmt.Errorf("%w: staged bytes changed", storage.ErrConflict)
			}
			if err = files.Remove(entry.path); err != nil {
				return err
			}
			if err = point(ctx, f, fmt.Sprintf("stage-cleanup:%d", i)); err != nil {
				return err
			}
		}
	}
	return nil
}

type stageWriter struct {
	ctx   context.Context
	out   io.Writer
	f     fault
	index int
}

func (w stageWriter) Write(b []byte) (int, error) {
	if err := point(w.ctx, w.f, fmt.Sprintf("before-stage-write:%d", w.index)); err != nil {
		return 0, err
	}
	n, err := w.out.Write(b)
	if err != nil {
		return n, err
	}
	return n, point(w.ctx, w.f, fmt.Sprintf("stage-write:%d", w.index))
}
