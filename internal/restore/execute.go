package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"reflect"
	"runtime"
	"strings"
	"time"

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
		guard, err := newGuard(ctx, r, w, files, &j, recheck, false)
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
		meta, err := openMeta(w.Common)
		if err != nil {
			return errors.Join(err, dropAnchor(ctx, r, w.Store, w.WorkspaceID, id, j.RecoverySnapshot))
		}
		defer meta.Close()
		// Until the journal exists nothing names the recovery ref, so a failure
		// here drops it instead of pointing the user at a transaction with no
		// journal.
		beforeJournal := func(err error) error {
			if _, statErr := meta.Lstat(journalPath(id)); statErr == nil {
				return stopped(meta, &j, err)
			}
			return errors.Join(err, dropAnchor(ctx, r, w.Store, w.WorkspaceID, id, j.RecoverySnapshot))
		}
		if err = guard.recognize(ctx); err != nil {
			return beforeJournal(err)
		}
		if err = point(ctx, f, "before-journal"); err != nil {
			return beforeJournal(err)
		}
		if err = meta.MkdirAll("transactions"); err != nil {
			return beforeJournal(err)
		}
		if err = writeJournal(meta, j); err != nil {
			return beforeJournal(err)
		}
		result.RecoverySnapshot = j.RecoverySnapshot
		if err = point(ctx, f, "journal"); err != nil {
			return stopped(meta, &j, err)
		}
		if err = run(ctx, r, w, files, meta, &j, guard, f); err != nil {
			return stopped(meta, &j, err)
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
	// evidence and branch are the main HEAD, index and branch every re-check
	// compares against: the plan's during execution, and a fresh snapshot
	// pinned when recovery starts.
	evidence selection.Evidence
	branch   string
	// images caches each path's last hashed images, by identity, with the
	// Lstat they were hashed under; see inspect.
	images map[string]map[string]cachedImage
	// index is the main index file; checked is the cheap global state seen at
	// the start of the last check of ownership, or nil when the next check
	// must be full; parents indexes the plan's parent directories and created the
	// directories it creates. See recognizeScope.
	index   string
	checked *globalState
	parents map[string]Parent
	created map[string]bool
}

// globalState is what can change the plan's preconditions outside the
// restored paths and is cheap to read: the main HEAD and index file
// (selection.MainState) and the branch. settled is false when a later
// reading might not show a change (see MainState.Settled).
type globalState struct {
	main    selection.MainState
	branch  string
	settled bool
}

func (s globalState) same(o globalState) bool { return s.branch == o.branch && s.main.Same(o.main) }

type cachedImage struct {
	info   fs.FileInfo // nil when the path didn't exist
	actual Fingerprint
}

// newGuard checks the operation and stage paths against main ownership. A
// recovery takes a fresh ownership snapshot: commits, checkouts and index
// changes since the interruption are fine while those paths stay unowned.
func newGuard(ctx context.Context, r *gitexec.Runner, w workspace.Context, files *storage.Dir, j *journal, recheck func() error, recovering bool) (*guard, error) {
	e, err := selection.New(r, w.Root, w.Admin)
	if err != nil {
		return nil, err
	}
	g := &guard{r: r, w: w, files: files, engine: e, journal: j, recheck: recheck, evidence: j.Manifest.Plan.Ownership, branch: j.Manifest.MainBranch, images: map[string]map[string]cachedImage{}, parents: map[string]Parent{}, created: map[string]bool{}}
	for _, parent := range j.Manifest.Plan.Parents {
		g.parents[parent.Path] = parent
	}
	for _, dir := range j.Manifest.Plan.CreateDirectories {
		g.created[dir] = true
	}
	if g.index, err = selection.IndexFile(ctx, r, w.Root, w.Admin); err != nil {
		e.Close()
		return nil, err
	}
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
	if recovering {
		g.evidence = owner.Evidence
		if g.branch, err = mainBranch(ctx, r, w); err != nil {
			e.Close()
			return nil, err
		}
	} else if owner.Evidence != j.Manifest.Plan.Ownership {
		e.Close()
		return nil, fmt.Errorf("%w: main ownership changed", storage.ErrConflict)
	}
	paths := make([]string, 0, 3*len(j.Manifest.Plan.Operations))
	for _, op := range j.Manifest.Plan.Operations {
		paths = append(paths, op.Path)
	}
	for _, stages := range j.Manifest.Stages {
		paths = append(paths, stages.Apply, stages.Rollback)
	}
	if err = owner.CheckPaths(paths); err != nil {
		e.Close()
		return nil, err
	}
	return g, nil
}
func (g *guard) close() { g.engine.Close() }

// pathChecked is called for each path a guard checks; tests replace it to
// count the checks.
var pathChecked = func(string) {}

// inspect returns path's current image. A full check re-checks every path,
// so it reuses an earlier hash while Lstat still shows the same file (same
// file identity, size, mode and modification time): a full check then costs
// a stat per path rather than a Git process. A path that changed is hashed
// again.
func (g *guard) inspect(ctx context.Context, path string, image Fingerprint, workspaceID, identityPath string) (Fingerprint, error) {
	pathChecked(path)
	key := ""
	identity := secureblob.Identity{RepositoryID: g.w.RepositoryID, WorkspaceID: workspaceID, Path: identityPath, Mode: image.GitMode}
	if image.KeyedDigest != "" {
		key = identity.WorkspaceID + "\x00" + identity.Path + "\x00" + identity.Mode
	}
	info, err := g.files.Lstat(path)
	if os.IsNotExist(err) {
		info, err = nil, nil
	}
	if err != nil {
		return Fingerprint{}, err
	}
	if cached, ok := g.images[path][key]; ok && sameStat(cached.info, info) {
		return cached.actual, nil
	}
	hashed := time.Now()
	var actual Fingerprint
	if image.KeyedDigest != "" {
		protected, e := inspectSensitiveFile(g.files, path, g.key, identity)
		err = e
		actual = protectedFingerprint(protected)
	} else {
		actual, err = inspectFile(ctx, g.r, g.w.Store, g.files, path)
	}
	if err != nil {
		return Fingerprint{}, err
	}
	if info == nil || !selection.Racy(info, hashed) {
		if g.images[path] == nil {
			g.images[path] = map[string]cachedImage{}
		}
		g.images[path][key] = cachedImage{info, actual}
	}
	return actual, nil
}

// forget drops cached images of path, so the next check hashes it again.
func (g *guard) forget(path string) { delete(g.images, path) }

// sameIdentity is selection.SameFile; tests replace it to model a filesystem
// whose change time is too coarse to show a rewrite.
var sameIdentity = selection.SameFile

func sameStat(a, b fs.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameIdentity(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
}

func (g *guard) matchesImage(ctx context.Context, path string, image Fingerprint, workspaceID, identityPath string) (bool, error) {
	actual, err := g.inspect(ctx, path, image, workspaceID, identityPath)
	return imageEqual(actual, image), err
}

// stageMatches is matchesImage for a reserved stage. It also accepts a
// complete regular-file stage still at its 0600 creation mode: a process that
// died after the last write but before StageNamed's Chmod leaves it there.
// created reports that case, so the caller applies the final mode.
func (g *guard) stageMatches(ctx context.Context, path string, image Fingerprint, workspaceID, identityPath string) (bool, bool, error) {
	actual, err := g.inspect(ctx, path, image, workspaceID, identityPath)
	if err != nil || imageEqual(actual, image) {
		return err == nil, false, err
	}
	if runtime.GOOS == "windows" || !image.Exists || image.Kind != "file" || !actual.Exists || actual.Kind != "file" || actual.Permissions != 0600 {
		return false, false, nil
	}
	actual.GitMode, actual.Permissions = image.GitMode, image.Permissions
	return imageEqual(actual, image), true, nil
}

// recognize checks every precondition of the plan: configuration, main
// ownership and branch, both workspace heads, and every operation's path,
// image and stages, and every parent directory.
func (g *guard) recognize(ctx context.Context) error { return g.recognizeScope(ctx, -1, "") }

// recognizeScope is recognize for one operation (op) or one created
// directory (dir). Checking every path for every operation made a restore
// quadratic in its files, so between full checks only the operation's own path
// (hashed again just before it changes), its stages and its ancestors are
// checked, with the configuration, both workspace heads and the cheap global
// state. Paths elsewhere can't be overwritten unchecked: each is checked again
// at its own operation, and a change to one already restored is found by the
// full check at the next phase boundary and at the end. Ownership can change
// only through HEAD or the index, so whenever the cheap global state differs
// from the last check of it, ownership (HEAD and the index listing) and the
// branch are checked again; a tool that rewrites the index without changing
// its entries then costs a listing, not every operation's image. The callers
// run a full check before the first change, at each phase boundary and at the
// end.
func (g *guard) recognizeScope(ctx context.Context, op int, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.recheck(); err != nil {
		return err
	}
	m := g.journal.Manifest
	p := m.Plan
	state, err := g.global(ctx)
	if err != nil {
		return err
	}
	full := op < 0 && dir == "" || g.checked == nil
	owned := full || !g.checked.settled || !state.same(*g.checked)
	if full {
		g.checked = nil
	}
	if owned {
		if err = (selection.Ownership{Evidence: g.evidence}).Revalidate(ctx, g.r, g.w.Root, g.w.Admin); err != nil {
			return err
		}
		if state.branch != g.branch {
			return fmt.Errorf("%w: main branch changed", storage.ErrConflict)
		}
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
	if full {
		// One pass reads each directory once (see CheckRestorePaths).
		paths := make([]string, 0, 3*len(p.Operations))
		for i, operation := range p.Operations {
			paths = append(paths, operation.Path, m.Stages[i].Apply, m.Stages[i].Rollback)
		}
		if err = g.engine.CheckRestorePaths(paths); err != nil {
			return err
		}
	}
	for i := range p.Operations {
		if full || i == op {
			if err = g.recognizeOperation(ctx, i, full); err != nil {
				return err
			}
		}
	}
	parents := p.Parents
	if !full {
		parents = nil
		target := dir
		if op >= 0 {
			target = p.Operations[op].Path
		}
		for ; target != "."; target = path.Dir(target) {
			if parent, ok := g.parents[target]; ok {
				parents = append(parents, parent)
			}
		}
	}
	for _, parent := range parents {
		pathChecked(parent.Path)
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
		if !parent.Exists && g.created[parent.Path] && now.Exists && now.Permissions == permissions {
			continue
		}
		return fmt.Errorf("%w: parent directory changed at %q", storage.ErrConflict, parent.Path)
	}
	// A racy index (or a HEAD MainState can't follow) could change again
	// without its stat showing it, so the next step reads ownership again;
	// it still checks only its own operation, or restore turns quadratic.
	if owned {
		state.settled = state.main.Settled()
		g.checked = &state
	}
	return nil
}

// global reads the cheap global state. It is read before the checks it
// stamps, so a change made during a full check shows at the next one.
func (g *guard) global(ctx context.Context) (globalState, error) {
	main, err := selection.ReadMainState(g.w.Admin, g.w.Common, g.index)
	if err != nil {
		return globalState{}, err
	}
	state := globalState{main: main}
	state.branch, err = mainBranch(ctx, g.r, g.w)
	return state, err
}

// recognizeOperation checks operation i's path, image and stages. A full
// check has checked the paths already (CheckRestorePaths); outside one, the
// ancestors are checked by stat only, and the directory scans for aliases and
// nested repositories, which cost a read of each ancestor directory, wait for
// the next full check. An alias created meanwhile can't be overwritten
// unchecked: on a filesystem that folds names, the operation's own image check
// reads it under the planned name.
func (g *guard) recognizeOperation(ctx context.Context, i int, full bool) error {
	m := g.journal.Manifest
	p := m.Plan
	op := p.Operations[i]
	check := g.engine.CheckRestoreAncestors
	if full {
		check = func(string) error { return nil }
	}
	pathChecked(op.Path)
	if err := check(op.Path); err != nil {
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
		return &externalEdit{path: op.Path, aside: !op.Before.Exists || !m.Post[i].Exists}
	}
	stages := m.Stages[i]
	for _, entry := range []struct {
		path  string
		image Fingerprint
	}{{stages.Apply, m.Post[i]}, {stages.Rollback, op.Before}} {
		pathChecked(entry.path)
		if err := check(entry.path); err != nil {
			return err
		}
		workspaceID := g.w.WorkspaceID
		if entry.path == stages.Apply {
			workspaceID = p.SourceWorkspaceID
		}
		match, _, e := g.stageMatches(ctx, entry.path, entry.image, workspaceID, op.Path)
		if e != nil {
			return e
		}
		info, e := g.files.Lstat(entry.path)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if info != nil && !match {
			return &unknownStage{path: entry.path}
		}
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
			if err := g.recognizeScope(ctx, -1, dir); err != nil {
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
	// Phase boundaries and the end check everything; see recognizeScope.
	if err := g.recognize(ctx); err != nil {
		return err
	}
	for i, op := range j.Manifest.Plan.Operations {
		desired := j.Manifest.Post[i]
		if rollback {
			desired = op.Before
		}
		// The path about to change is hashed again, not taken from the cache.
		g.forget(op.Path)
		if err := g.recognizeScope(ctx, i, ""); err != nil {
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
		// Progress is a hint (recovery recognizes each path's image), and the
		// journal holds every operation, so writing it after each one made a
		// restore quadratic in its files. It is written when the count reaches
		// a power of two and after the last operation.
		j.Progress = i + 1
		if j.Progress&(j.Progress-1) == 0 || j.Progress == len(j.Manifest.Plan.Operations) {
			if err = writeJournal(meta, *j); err != nil {
				return err
			}
		}
		if err = point(ctx, f, fmt.Sprintf("progress:%d", i)); err != nil {
			return err
		}
	}
	if err := g.recognize(ctx); err != nil {
		return err
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
			if err := g.recognizeScope(ctx, -1, dir); err != nil {
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
			match, created, e := g.stageMatches(ctx, stage, desired, identity.WorkspaceID, path)
			if e != nil {
				return e
			}
			if !match {
				return &unknownStage{path: stage}
			}
			if created {
				if err := files.ChmodStaged(stage, os.FileMode(desired.Permissions)); err != nil {
					return err
				}
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
	// The destination and stage are hashed again, not taken from the cache,
	// immediately before the rename: staging may have taken long enough for
	// another program to rewrite the destination.
	g.forget(path)
	g.forget(stage)
	if err := g.recognizeScope(ctx, i, ""); err != nil {
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
		g, err := newGuard(ctx, r, w, files, &j, recheck, true)
		if err != nil {
			return stopped(meta, &j, err)
		}
		defer g.close()
		if err = g.recognize(ctx); err != nil {
			return stopped(meta, &j, err)
		}
		// What stopped it last is resolved.
		if j.Stopped != nil {
			j.Stopped = nil
			if err = writeJournal(meta, j); err != nil {
				return stopped(meta, &j, err)
			}
		}
		if rollback {
			if err = point(ctx, f, "before-rollback-intent"); err != nil {
				return stopped(meta, &j, err)
			}
			j.Direction = "rollback"
			j.Progress = 0
			if err = writeJournal(meta, j); err != nil {
				return stopped(meta, &j, err)
			}
			if err = point(ctx, f, "rollback-intent"); err != nil {
				return stopped(meta, &j, err)
			}
		}
		if err = run(ctx, r, w, files, meta, &j, g, f); err != nil {
			return stopped(meta, &j, err)
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
			match, _, err := g.stageMatches(ctx, entry.path, entry.image, workspaceID, g.journal.Manifest.Plan.Operations[i].Path)
			if err != nil {
				return err
			}
			if !match {
				return &unknownStage{path: entry.path}
			}
			if err = point(ctx, f, fmt.Sprintf("before-stage-cleanup:%d", i)); err != nil {
				return err
			}
			if err = g.recognizeScope(ctx, i, ""); err != nil {
				return err
			}
			match, _, e := g.stageMatches(ctx, entry.path, entry.image, workspaceID, g.journal.Manifest.Plan.Operations[i].Path)
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

// Abandon gives up a pending restore without resuming or rolling it back.
// Working files stay as they are and the recovery ref is kept, so the anchored
// preimages and postimages remain in local history. A staging file is removed
// when it matches its anchored image; any other staging bytes are quarantined.
func Abandon(ctx context.Context, r *gitexec.Runner, cwd, id string) (configtx.AbandonResult, error) {
	result := configtx.AbandonResult{TransactionID: id, Kind: "restore"}
	if !storage.ValidUUID(id) {
		return result, errors.New("invalid restore transaction ID")
	}
	err := configtx.WithRestore(ctx, r, cwd, id, func(w workspace.Context, state selection.State, digest string, _ func() error) error {
		meta, err := openMeta(w.Common)
		if err != nil {
			return err
		}
		defer meta.Close()
		j, err := readJournal(meta, journalPath(id))
		if os.IsNotExist(err) {
			done, e := readJournal(meta, completedPath(id))
			if e != nil {
				return e
			}
			if done.Outcome != configtx.OutcomeAbandoned {
				return errors.New("restore already completed with a different outcome")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if destination := j.Manifest.Plan.DestinationWorkspaceID; destination != w.WorkspaceID {
			return fmt.Errorf("%w: abandon restore transaction %s from its destination workspace %s", storage.ErrConflict, id, destination)
		}
		// The anchor must hold every recorded image before a matching stage can
		// be deleted as a duplicate.
		evidence := j
		evidence.Outcome = configtx.OutcomeAbandoned
		if err := validateJournalWithLiveState(ctx, r, w, state, digest, id, evidence, false); err != nil {
			return err
		}
		var key []byte
		var keyErr error
		if j.SchemaVersion == 2 {
			policy, loaded, e := loadRestoreKey(w)
			if e == nil && policy.KeyFingerprint != j.Manifest.Plan.EncryptedKeyFingerprint {
				e = errors.New("sensitive-file key differs from the restore journal")
			}
			key, keyErr = loaded, e
			if e != nil {
				key = nil
			}
		}
		files, err := storage.OpenDir(w.Root)
		if err != nil {
			return err
		}
		defer files.Close()
		q, err := configtx.NewQuarantine(meta, id, "restore")
		if err != nil {
			return err
		}
		g := &guard{r: r, w: w, files: files, journal: &j, key: key, images: map[string]map[string]cachedImage{}}
		m := j.Manifest
		for i, stages := range m.Stages {
			op := m.Plan.Operations[i]
			for _, entry := range []struct {
				path, workspaceID string
				image             Fingerprint
			}{{stages.Apply, m.Plan.SourceWorkspaceID, m.Post[i]}, {stages.Rollback, w.WorkspaceID, op.Before}} {
				if err := ctx.Err(); err != nil {
					return err
				}
				if _, err := files.Lstat(entry.path); os.IsNotExist(err) {
					continue
				} else if err != nil {
					return err
				}
				reason := "staging bytes don't match the anchored image"
				if entry.image.KeyedDigest == "" || key != nil {
					match, _, err := g.stageMatches(ctx, entry.path, entry.image, entry.workspaceID, op.Path)
					if err != nil {
						return err
					}
					if match {
						if err := files.Remove(entry.path); err != nil {
							return err
						}
						result.Removed = append(result.Removed, entry.path)
						continue
					}
				} else {
					reason = "sensitive-file key is unavailable to verify the staging bytes"
				}
				quarantined, err := q.QuarantineFile(meta, files, entry.path, w.Root, reason)
				if err != nil {
					return err
				}
				result.Quarantined = append(result.Quarantined, quarantined)
			}
		}
		if keyErr != nil && len(result.Quarantined) > 0 {
			result.Warnings = append(result.Warnings, "sensitive-file key unavailable: "+keyErr.Error())
		}
		result.Quarantine = q.Directory(w.Common)
		j.Outcome = configtx.OutcomeAbandoned
		// Earlier releases decode completed journals strictly and don't know
		// the field; abandoning resolves the stop.
		j.Stopped = nil
		if err := writeJournal(meta, j); err != nil {
			return err
		}
		return archive(ctx, meta, j, nil)
	})
	return result, err
}
