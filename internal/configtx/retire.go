package configtx

import (
	"context"
	"errors"
	"path/filepath"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func recordPath(id string) string { return filepath.Join("workspaces", id, "workspace.json") }
func prepareRetirement(s *session, src Source, before Image) (Journal, error) {
	record, err := src.record()
	if err != nil {
		return Journal{}, err
	}
	if record.State != "active" {
		return Journal{}, errors.New("retirement requires an active workspace record")
	}
	j, err := prepare(s, src, selection.Empty(), before, false)
	if err != nil {
		return Journal{}, err
	}
	record.State = "retained"
	data, err := encode(record)
	if err != nil {
		return Journal{}, err
	}
	j.Kind = "retirement"
	j.Operation = "retire"
	j.Before = src.Record
	j.After = image(data, true, src.Record.Mode)
	return j, nil
}
func Retire(ctx context.Context, r *gitexec.Runner, cwd, id string) (Result, error) {
	return retire(ctx, r, cwd, id, nil)
}
func retire(ctx context.Context, r *gitexec.Runner, cwd, id string, f fault) (Result, error) {
	if !storage.ValidUUID(id) {
		return Result{}, errors.New("invalid workspace UUID")
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
	if len(pending) > 0 {
		return Result{}, pendingError(pending[0])
	}
	var target Source
	for _, src := range s.sources {
		if src.ID == id {
			target = src
		}
	}
	if target.ID == "" {
		return Result{}, errors.New("unknown workspace UUID")
	}
	if err = s.retireEligible(ctx, id); err != nil {
		return Result{}, err
	}
	record, err := target.record()
	if err != nil {
		return Result{}, err
	}
	result := Result{WorkspaceID: id}
	if record.State == "retained" {
		return result, nil
	}
	before, err := readImage(s.common, "info/exclude")
	if err != nil {
		return result, err
	}
	j, err := prepareRetirement(s, target, before)
	if err != nil {
		return result, err
	}
	result.TransactionID = j.ID
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
	result.Warnings, err = s.apply(ctx, &j, f)
	if err != nil {
		return result, withRecovery(j.ID, err)
	}
	result.Changed = true
	result.Warnings = uniqueWarnings(result.Warnings)
	return result, nil
}
func (s *session) verifyRetirement(ctx context.Context, j Journal) ([]string, error) {
	before := s.sources
	s.sources = append([]Source{}, before...)
	defer func() { s.sources = before }()
	for i := range s.sources {
		if s.sources[i].ID == j.WorkspaceID {
			s.sources[i].Record = j.After
		}
	}
	// Retirement removes rules, like untrack; unrelated existing conflicts warn.
	return s.verify(ctx, "", selection.Empty(), true)
}
func (s *session) applyRetirement(ctx context.Context, j *Journal, f fault) ([]string, error) {
	if j.Direction != "apply" {
		return nil, errors.New("rollback has started; use --rollback")
	}
	if j.Outcome == "committed" {
		return nil, s.archive(ctx, *j, f)
	}
	if j.Outcome != "" {
		return nil, errors.New("retirement already rolled back")
	}
	if err := s.retireEligible(ctx, j.WorkspaceID); err != nil {
		return nil, err
	}
	if err := s.install(ctx, *j, j.ExcludeUnion, false, f, "union"); err != nil {
		return nil, err
	}
	if err := s.retireEligible(ctx, j.WorkspaceID); err != nil {
		return nil, err
	}
	if err := s.install(ctx, *j, j.After, true, f, "selection"); err != nil {
		return nil, err
	}
	if err := s.retireEligible(ctx, j.WorkspaceID); err != nil {
		return nil, err
	}
	if err := s.install(ctx, *j, j.ExcludeAfter, false, f, "final-exclude"); err != nil {
		return nil, err
	}
	warnings, err := s.verifyRetirement(ctx, *j)
	if err != nil {
		return warnings, err
	}
	if err = s.recognize(*j); err != nil {
		return warnings, err
	}
	if err = s.retireEligible(ctx, j.WorkspaceID); err != nil {
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
func validateRetirement(j Journal, repo, id string) error {
	if j.SchemaVersion != 1 || j.Operation != "retire" || j.ID != id || j.RepositoryID != repo || !storage.ValidUUID(j.WorkspaceID) || (j.Direction != "apply" && j.Direction != "rollback") || (j.Outcome != "" && j.Outcome != "committed" && j.Outcome != "rolledback" && j.Outcome != OutcomeAbandoned) {
		return errors.New("invalid retirement journal")
	}
	var target Source
	last := ""
	for _, src := range j.Sources {
		if src.ID <= last {
			return errors.New("unordered retirement sources")
		}
		last = src.ID
		if err := validateSource(src); err != nil {
			return err
		}
		if src.ID == j.WorkspaceID {
			target = src
		}
	}
	if target.ID == "" || !same(target.Record, j.Before) {
		return errors.New("retirement target mismatch")
	}
	for _, im := range []Image{j.Before, j.After, j.ExcludeBefore, j.ExcludeUnion, j.ExcludeAfter} {
		if !same(im, image(im.Data, im.Exists, im.Mode)) || im.Mode > 0777 || !im.Exists && (len(im.Data) > 0 || im.Mode != 0) {
			return errors.New("invalid retirement image")
		}
	}
	s := &session{sources: j.Sources, w: &workspace.Context{RepositoryID: repo}}
	expected, err := prepareRetirement(s, target, j.ExcludeBefore)
	if err != nil {
		return err
	}
	if !same(j.Before, expected.Before) || !same(j.After, expected.After) || !same(j.ExcludeUnion, expected.ExcludeUnion) || !same(j.ExcludeAfter, expected.ExcludeAfter) {
		return errors.New("retirement journal images do not match recorded rules")
	}
	return nil
}
