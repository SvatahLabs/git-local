package configtx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type Journal struct {
	SchemaVersion int      `json:"schemaVersion"`
	Kind          string   `json:"kind"`
	ID            string   `json:"id"`
	RepositoryID  string   `json:"repositoryId"`
	WorkspaceID   string   `json:"workspaceId"`
	Operation     string   `json:"operation"`
	Direction     string   `json:"direction"`
	Outcome       string   `json:"outcome,omitempty"`
	Sources       []Source `json:"sources"`
	Before        Image    `json:"selectionBefore"`
	After         Image    `json:"selectionAfter"`
	ExcludeBefore Image    `json:"excludeBefore"`
	ExcludeUnion  Image    `json:"excludeUnion"`
	ExcludeAfter  Image    `json:"excludeAfter"`
}
type Result struct {
	WorkspaceID, TransactionID string
	Changed                    bool
	Warnings                   []string
}

func journalPath(id string) string   { return "transactions/config-" + id + ".json" }
func completedPath(id string) string { return "transactions/completed/config-" + id + ".json" }

type fault func(string) error

func point(ctx context.Context, f fault, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f != nil {
		return f(name)
	}
	return nil
}

func Change(ctx context.Context, r *gitexec.Runner, cwd string, track bool, args ...string) (Result, error) {
	return change(ctx, r, cwd, track, nil, args...)
}

type Planner func(workspace.Context, selection.State) (selection.State, error)

// ChangePlanned runs source resolution and destination planning under the common lock.
func ChangePlanned(ctx context.Context, r *gitexec.Runner, cwd string, planner Planner) (Result, error) {
	return changePlanned(ctx, r, cwd, true, nil, planner)
}
func change(ctx context.Context, r *gitexec.Runner, cwd string, track bool, f fault, args ...string) (Result, error) {
	return changePlanned(ctx, r, cwd, track, f, nil, args...)
}
func changePlanned(ctx context.Context, r *gitexec.Runner, cwd string, track bool, f fault, planner Planner, args ...string) (Result, error) {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return Result{}, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return Result{}, err
	}
	if len(pending) > 0 {
		return Result{}, pendingError(pending[0])
	}
	var current Source
	for _, src := range s.sources {
		if src.ID == s.w.WorkspaceID {
			current = src
		}
	}
	old, err := current.state()
	if err != nil {
		return Result{}, err
	}
	engine, err := selection.New(r, s.w.Root, s.w.Admin)
	if err != nil {
		return Result{}, err
	}
	defer engine.Close()
	var next selection.State
	if planner != nil {
		next, err = planner(*s.w, old)
		if err != nil {
			return Result{}, err
		}
	} else if track {
		plan, e := engine.PlanTrack(ctx, old, cwd, args...)
		if e != nil {
			return Result{}, e
		}
		next = plan.State
	} else {
		next, err = engine.PlanUntrack(old, cwd, args...)
		if err != nil {
			return Result{}, err
		}
	}
	beforeIgnore, err := readImage(s.common, "info/exclude")
	if err != nil {
		return Result{}, err
	}
	j, err := prepare(s, current, next, beforeIgnore, track)
	if err != nil {
		return Result{}, err
	}
	result := Result{WorkspaceID: current.ID, TransactionID: j.ID}
	if same(j.Before, j.After) && same(j.ExcludeBefore, j.ExcludeAfter) {
		result.TransactionID = ""
		result.Warnings, err = s.verify(ctx, current.ID, next, !track)
		return result, err
	}
	if err = point(ctx, f, "before-journal"); err != nil {
		return result, err
	}
	if err = s.meta.MkdirAll("transactions"); err != nil {
		return result, err
	}
	if err = s.writeJournal(j); err != nil {
		return result, err
	}
	if err = point(ctx, f, "journal"); err != nil {
		return result, withRecovery(j.ID, err)
	}
	warnings, err := s.apply(ctx, &j, f)
	result.Warnings = warnings
	if err != nil {
		if isValidation(err) && ctx.Err() == nil {
			_, rollbackErr := s.rollback(ctx, &j, nil)
			if rollbackErr == nil {
				return result, fmt.Errorf("selection update rejected and rolled back: %w", err)
			}
			return result, withRecovery(j.ID, fmt.Errorf("verification failed; rollback also failed: %w", rollbackErr))
		}
		return result, withRecovery(j.ID, err)
	}
	result.Changed = true
	result.Warnings = uniqueWarnings(result.Warnings)
	return result, nil
}
func prepare(s *session, current Source, next selection.State, beforeIgnore Image, track bool) (Journal, error) {
	oldStates, err := states(s.sources, "", nil)
	if err != nil {
		return Journal{}, err
	}
	newStates, err := states(s.sources, current.ID, &next)
	if err != nil {
		return Journal{}, err
	}
	oldPatterns, oldAlternates, err := s.compile(oldStates)
	if err != nil {
		return Journal{}, err
	}
	newPatterns, _, err := s.compile(newStates)
	if err != nil {
		return Journal{}, err
	}
	both, _, err := s.compile(append(append([]selection.State{}, oldStates...), newStates...))
	if err != nil {
		return Journal{}, err
	}
	user, err := exclude.UserBytes(beforeIgnore.Data, oldPatterns, oldAlternates...)
	if err != nil {
		return Journal{}, err
	}
	afterBytes, err := encode(next)
	if err != nil {
		return Journal{}, err
	}
	id, err := storage.UUID()
	if err != nil {
		return Journal{}, err
	}
	mode := beforeIgnore.Mode
	if !beforeIgnore.Exists {
		mode = 0600
	}
	makeIgnore := func(patterns []byte) Image {
		data := exclude.Render(user, patterns)
		exists := beforeIgnore.Exists || len(data) > 0
		if !exists {
			return image(nil, false, 0)
		}
		return image(data, true, mode)
	}
	op := "untrack"
	if track {
		op = "track"
	}
	j := Journal{SchemaVersion: 1, Kind: "selection", ID: id, RepositoryID: s.w.RepositoryID, WorkspaceID: current.ID, Operation: op, Direction: "apply", Sources: s.sources, Before: current.Selection, After: image(afterBytes, true, 0600), ExcludeBefore: beforeIgnore, ExcludeUnion: makeIgnore(both), ExcludeAfter: makeIgnore(newPatterns)}
	if _, err = encodeNew(j); err != nil {
		return Journal{}, err
	}
	return j, nil
}
func pendingError(id string) error {
	return fmt.Errorf("%w: unfinished transaction %s; run git local recover %s --resume or --rollback", storage.ErrConflict, id, id)
}
func withRecovery(id string, err error) error {
	return fmt.Errorf("%w: transaction %s stopped (%v); run git local recover %s --resume or --rollback", storage.ErrConflict, id, err, id)
}
func (s *session) writeJournal(j Journal) error {
	b, err := encode(j)
	if err != nil {
		return err
	}
	return s.meta.AtomicWrite(journalPath(j.ID), b, 0600)
}

// recognize validates all registry inputs and every destination image before
// each write. An arbitrary editor's new bytes are a conflict, never overwritten.
func (s *session) recognize(j Journal) error {
	actual, err := s.readSources()
	if err != nil {
		return err
	}
	if len(actual) != len(j.Sources) {
		return fmt.Errorf("%w: workspace registrations changed", storage.ErrConflict)
	}
	for i, src := range actual {
		expected := j.Sources[i]
		if j.Kind == "retirement" && src.ID == j.WorkspaceID {
			if src.ID != expected.ID || !same(src.Selection, expected.Selection) || (!same(src.Record, j.Before) && !same(src.Record, j.After)) {
				return fmt.Errorf("%w: retirement inputs were externally edited", storage.ErrConflict)
			}
			continue
		}
		if src.ID != expected.ID || !same(src.Record, expected.Record) {
			return fmt.Errorf("%w: workspace registration changed", storage.ErrConflict)
		}
		if src.ID == j.WorkspaceID {
			if !same(src.Selection, j.Before) && !same(src.Selection, j.After) {
				return fmt.Errorf("%w: selection was externally edited", storage.ErrConflict)
			}
		} else if !same(src.Selection, expected.Selection) {
			return fmt.Errorf("%w: another workspace selection changed", storage.ErrConflict)
		}
	}
	current, err := readImage(s.common, "info/exclude")
	if err != nil {
		return err
	}
	if !same(current, j.ExcludeBefore) && !same(current, j.ExcludeUnion) && !same(current, j.ExcludeAfter) {
		return fmt.Errorf("%w: info/exclude was externally edited; reconcile it with the recorded transaction before recovery", storage.ErrConflict)
	}
	return nil
}
func (s *session) install(ctx context.Context, j Journal, desired Image, selectionFile bool, f fault, name string) error {
	if err := point(ctx, f, "before-"+name); err != nil {
		return err
	}
	if err := s.recognize(j); err != nil {
		return err
	}
	d, path := s.common, "info/exclude"
	if selectionFile {
		d, path = s.meta, selectionPath(j.WorkspaceID)
		if j.Kind == "retirement" {
			path = recordPath(j.WorkspaceID)
		}
	}
	now, err := readImage(d, path)
	if err != nil {
		return err
	}
	if !same(now, desired) {
		if desired.Exists {
			if !selectionFile {
				if err = s.common.MkdirAll("info"); err != nil {
					return err
				}
			}
			err = d.AtomicWrite(path, desired.Data, os.FileMode(desired.Mode))
		} else {
			err = d.Remove(path)
		}
		if err != nil {
			return err
		}
	}
	return point(ctx, f, name)
}
func (s *session) apply(ctx context.Context, j *Journal, f fault) ([]string, error) {
	if j.Kind == "retirement" {
		return s.applyRetirement(ctx, j, f)
	}
	if j.Direction != "apply" {
		return nil, errors.New("rollback has started; use --rollback to finish it")
	}
	if j.Outcome == "committed" {
		return nil, s.archive(ctx, *j, f)
	}
	if j.Outcome != "" {
		return nil, errors.New("transaction was rolled back")
	}
	if err := s.install(ctx, *j, j.ExcludeUnion, false, f, "union"); err != nil {
		return nil, err
	}
	var next selection.State
	if err := decode(j.After.Data, &next); err != nil {
		return nil, err
	}
	warnings, err := s.verify(ctx, j.WorkspaceID, next, j.Operation == "untrack")
	if err != nil {
		return warnings, err
	}
	if err = s.install(ctx, *j, j.After, true, f, "selection"); err != nil {
		return warnings, err
	}
	if err = s.install(ctx, *j, j.ExcludeAfter, false, f, "final-exclude"); err != nil {
		return warnings, err
	}
	more, err := s.verify(ctx, j.WorkspaceID, next, j.Operation == "untrack")
	warnings = append(warnings, more...)
	if err != nil {
		return warnings, err
	}
	if err = s.recognize(*j); err != nil {
		return warnings, err
	}
	j.Outcome = "committed"
	if err = s.writeJournal(*j); err != nil {
		return warnings, err
	}
	if err = point(ctx, f, "committed"); err != nil {
		return warnings, err
	}
	return warnings, s.archive(ctx, *j, f)
}
func (s *session) rollback(ctx context.Context, j *Journal, f fault) ([]string, error) {
	if j.Outcome == "committed" {
		return nil, errors.New("transaction already committed; use --resume to finish bookkeeping")
	}
	if j.Outcome == "rolledback" {
		return nil, s.archive(ctx, *j, f)
	}
	if err := s.recognize(*j); err != nil {
		return nil, err
	}
	j.Direction = "rollback"
	if err := s.writeJournal(*j); err != nil {
		return nil, err
	}
	if err := point(ctx, f, "rollback-intent"); err != nil {
		return nil, err
	}
	if err := s.install(ctx, *j, j.ExcludeUnion, false, f, "rollback-union"); err != nil {
		return nil, err
	}
	if err := s.install(ctx, *j, j.Before, true, f, "rollback-selection"); err != nil {
		return nil, err
	}
	if err := s.install(ctx, *j, j.ExcludeBefore, false, f, "rollback-exclude"); err != nil {
		return nil, err
	}
	if err := s.recognize(*j); err != nil {
		return nil, err
	}
	j.Outcome = "rolledback"
	if err := s.writeJournal(*j); err != nil {
		return nil, err
	}
	if err := point(ctx, f, "rolledback"); err != nil {
		return nil, err
	}
	return nil, s.archive(ctx, *j, f)
}
func (s *session) archive(ctx context.Context, j Journal, f fault) error {
	if err := point(ctx, f, "before-archive"); err != nil {
		return err
	}
	if err := s.meta.MkdirAll("transactions/completed"); err != nil {
		return err
	}
	if _, err := s.meta.Lstat(completedPath(j.ID)); err == nil {
		return fmt.Errorf("%w: completed transaction already exists", storage.ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := s.meta.Rename(journalPath(j.ID), completedPath(j.ID)); err != nil {
		return err
	}
	return point(ctx, f, "archived")
}

func Pending(ctx context.Context, r *gitexec.Runner, cwd string) ([]string, error) {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return s.pending()
}
func Recover(ctx context.Context, r *gitexec.Runner, cwd, id string, rollback bool) (Result, error) {
	return recoverTransaction(ctx, r, cwd, id, rollback, nil)
}
func recoverTransaction(ctx context.Context, r *gitexec.Runner, cwd, id string, rollback bool, f fault) (Result, error) {
	if !storage.ValidUUID(id) {
		return Result{}, errors.New("invalid transaction ID")
	}
	s, err := open(ctx, r, cwd)
	if err != nil {
		return Result{}, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return Result{}, err
	}
	for _, other := range pending {
		if other != id {
			return Result{}, pendingError(other)
		}
	}
	var j Journal
	err = s.meta.ReadJSON(journalPath(id), &j)
	completed := false
	if os.IsNotExist(err) {
		err = s.meta.ReadJSON(completedPath(id), &j)
		completed = true
	}
	if err != nil {
		return Result{}, err
	}
	if err = validateJournal(j, s.w.RepositoryID, id); err != nil {
		return Result{}, err
	}
	result := Result{WorkspaceID: j.WorkspaceID, TransactionID: id}
	if completed {
		if rollback && j.Outcome == "rolledback" || !rollback && j.Outcome == "committed" {
			return result, nil
		}
		return result, errors.New("transaction already completed with a different outcome")
	}
	if err = s.recognize(j); err != nil {
		return result, withRecovery(id, err)
	}
	// Verification must use the immutable original roster, substituting After for
	// the target. Actual selection may already be After after an interrupted write.
	s.sources = j.Sources
	if rollback {
		result.Warnings, err = s.rollback(ctx, &j, f)
	} else {
		result.Warnings, err = s.apply(ctx, &j, f)
	}
	if err != nil {
		return result, withRecovery(id, err)
	}
	result.Changed = true
	result.Warnings = uniqueWarnings(result.Warnings)
	return result, nil
}
func validateJournal(j Journal, repo, id string) error {
	if j.Kind == "retirement" {
		return validateRetirement(j, repo, id)
	}
	if j.SchemaVersion != 1 || j.Kind != "selection" || j.ID != id || j.RepositoryID != repo || !storage.ValidUUID(j.WorkspaceID) || (j.Operation != "track" && j.Operation != "untrack") || (j.Direction != "apply" && j.Direction != "rollback") || (j.Outcome != "" && j.Outcome != "committed" && j.Outcome != "rolledback" && j.Outcome != OutcomeAbandoned) {
		return errors.New("invalid configuration transaction journal")
	}
	for _, im := range []Image{j.Before, j.After, j.ExcludeBefore, j.ExcludeUnion, j.ExcludeAfter} {
		if !same(im, image(im.Data, im.Exists, im.Mode)) || im.Mode > 0777 || !im.Exists && (len(im.Data) > 0 || im.Mode != 0) {
			return errors.New("invalid journal file image")
		}
	}
	var next selection.State
	if !j.Before.Exists || !j.After.Exists {
		return errors.New("missing journal selection image")
	}
	if err := decode(j.After.Data, &next); err != nil {
		return err
	}
	canonical, err := selection.Canonical(next)
	if err != nil {
		return err
	}
	canonicalBytes, err := encode(canonical)
	if err != nil || !bytes.Equal(canonicalBytes, j.After.Data) {
		return errors.New("noncanonical journal selection")
	}
	var target Source
	last := ""
	for _, src := range j.Sources {
		if src.ID <= last {
			return errors.New("unordered or duplicate journal sources")
		}
		last = src.ID
		if err = validateSource(src); err != nil {
			return err
		}
		if src.ID == j.WorkspaceID {
			target = src
		}
	}
	if target.ID == "" || !same(target.Selection, j.Before) {
		return errors.New("journal target does not match registry")
	}
	// Recompute all generated bytes. A journal cannot smuggle unrelated writes into
	// the ignore file or substitute arbitrary user content during recovery.
	oldStates, err := states(j.Sources, "", nil)
	if err != nil {
		return err
	}
	newStates, err := states(j.Sources, j.WorkspaceID, &next)
	if err != nil {
		return err
	}
	// The journal may have been written under either core.ignorecase setting.
	oldPatterns, oldAlternate, err := compileEither(oldStates)
	if err != nil {
		return err
	}
	user, err := exclude.UserBytes(j.ExcludeBefore.Data, oldPatterns, oldAlternate)
	if err != nil {
		return err
	}
	patterns, alternate, err := compileEither(newStates)
	if err != nil {
		return err
	}
	union, unionAlternate, err := compileEither(append(oldStates, newStates...))
	if err != nil {
		return err
	}
	if !exclude.Rendered(j.ExcludeAfter.Data, user, patterns, alternate) || !exclude.Rendered(j.ExcludeUnion.Data, user, union, unionAlternate) {
		return errors.New("journal ignore images do not match selection rules")
	}
	return nil
}

// compileEither compiles states for both core.ignorecase settings.
func compileEither(states []selection.State) ([]byte, []byte, error) {
	sensitive, err := exclude.Compile(states, false)
	if err != nil {
		return nil, nil, err
	}
	folded, err := exclude.Compile(states, true)
	return sensitive, folded, err
}
