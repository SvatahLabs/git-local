package configtx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// JournalInfo names an existing transaction. A configuration journal is checked
// against its schema and original images; restore journal content is checked by
// the restore package, which knows its anchored recovery snapshot.
type JournalInfo struct {
	ID        string
	Kind      string
	Completed bool
	Err       error
}

type DiagnosticState struct {
	Workspace      workspace.Context
	Selection      selection.State
	Sources        []string
	Workspaces     []WorkspaceInfo
	InventoryErr   error
	Pending        []string
	PendingErr     error
	Journals       []JournalInfo
	JournalErr     error
	ConfigDigest   string
	ConfigErr      error
	IgnoreWarnings []string
	IgnoreErr      error
	// IgnoreOutdated reports a valid managed ignore block that isn't the
	// current one: written before it covered restore and seed staging files,
	// or compiled for the other core.ignorecase setting.
	IgnoreOutdated bool
	Recheck        func() error
}

// ReadDiagnostic holds an existing shared lock and never replays a pending
// transaction. The callback can inspect the private store under that lock.
func ReadDiagnostic(ctx context.Context, r *gitexec.Runner, cwd string, fn func(DiagnosticState) error) error {
	s, err := openMode(ctx, r, cwd, true)
	if err != nil {
		return err
	}
	defer s.close()
	d := DiagnosticState{Workspace: *s.w, Sources: []string{}, Pending: []string{}, Journals: []JournalInfo{}, IgnoreWarnings: []string{}}
	for _, src := range s.sources {
		d.Sources = append(d.Sources, src.ID)
		if src.ID == s.w.WorkspaceID {
			d.Selection, err = src.state()
			if err != nil {
				return err
			}
		}
	}
	d.Pending, d.PendingErr = s.pending()
	d.Workspaces, d.InventoryErr = s.inventory(ctx)
	d.Journals, d.JournalErr = s.inspectJournals()
	d.ConfigDigest, d.ConfigErr = s.stateDigest()
	if d.ConfigErr == nil {
		d.IgnoreWarnings, d.IgnoreErr = s.verify(ctx, s.w.WorkspaceID, d.Selection, false)
		d.IgnoreOutdated, err = s.ignoreOutdated()
		if err != nil {
			return err
		}
	} else {
		d.IgnoreErr = d.ConfigErr
	}
	d.Recheck = func() error {
		current, e := s.readSources()
		if e != nil {
			return e
		}
		if len(current) != len(s.sources) {
			return fmt.Errorf("%w: workspace configuration changed during diagnosis", storage.ErrConflict)
		}
		for i, src := range current {
			old := s.sources[i]
			if src.ID != old.ID || !same(src.Record, old.Record) || !same(src.Selection, old.Selection) {
				return fmt.Errorf("%w: workspace configuration changed during diagnosis", storage.ErrConflict)
			}
		}
		if d.ConfigErr == nil {
			now, e := s.stateDigest()
			if e != nil {
				return e
			}
			if now != d.ConfigDigest {
				return fmt.Errorf("%w: ignore configuration changed during diagnosis", storage.ErrConflict)
			}
		}
		return nil
	}
	return fn(d)
}
func (s *session) inspectJournals() ([]JournalInfo, error) {
	out := []JournalInfo{}
	for _, directory := range []struct {
		path      string
		completed bool
	}{{"transactions", false}, {"transactions/completed", true}} {
		entries, err := s.meta.ReadDir(directory.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if directory.path == "transactions" && name == "completed" && entry.IsDir() {
				continue
			}
			if storage.IsTempName(name) && entry.Type().IsRegular() {
				continue
			}
			if !entry.Type().IsRegular() {
				return out, fmt.Errorf("%w: unexpected transaction state %q; inspect manually", storage.ErrConflict, name)
			}
			kind, id, ok := journalName(name)
			if !ok {
				return out, fmt.Errorf("%w: unknown transaction %q; run git local init for an unfinished initialization", storage.ErrConflict, name)
			}
			item := JournalInfo{ID: id, Kind: kind, Completed: directory.completed}
			if kind == "config" {
				var j Journal
				item.Err = s.meta.ReadJSON(directory.path+"/"+name, &j)
				if item.Err == nil {
					item.Err = validateJournal(j, s.w.RepositoryID, id)
				}
				if item.Err == nil && directory.completed && j.Outcome == "" {
					item.Err = errors.New("completed configuration journal has no outcome")
				}
			}
			if (kind == "encrypt" || kind == "init") && directory.completed {
				// Only recover --abandon archives these; record their outcome.
				var j struct {
					Outcome string `json:"outcome"`
				}
				data, err := s.meta.ReadFile(directory.path+"/"+name, 16<<20)
				item.Err = err
				if item.Err == nil {
					item.Err = json.Unmarshal(data, &j)
				}
				if item.Err == nil && j.Outcome != OutcomeAbandoned {
					item.Err = errors.New("completed " + kind + " journal was not abandoned")
				}
			}
			if kind == "seed" {
				var j seedJournal
				item.Err = s.meta.ReadJSON(directory.path+"/"+name, &j)
				if item.Err == nil {
					item.Err = validateSeed(j, s.w.RepositoryID, j.WorkspaceID)
				}
				if item.Err == nil && directory.completed && j.Outcome == "" {
					item.Err = errors.New("completed seed journal has no outcome")
				}
			}
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return !out[i].Completed && out[j].Completed
	})
	for i := 1; i < len(out); i++ {
		if out[i].ID == out[i-1].ID {
			return out, fmt.Errorf("%w: duplicate journal ID %s", storage.ErrConflict, out[i].ID)
		}
	}
	return out, nil
}
func journalName(name string) (string, string, bool) {
	for _, item := range []struct{ prefix, kind string }{{"config-", "config"}, {"restore-", "restore"}, {"seed-", "seed"}, {"encrypt-", "encrypt"}, {"init-", "init"}} {
		if strings.HasPrefix(name, item.prefix) && strings.HasSuffix(name, ".json") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, item.prefix), ".json")
			return item.kind, id, storage.ValidUUID(id)
		}
	}
	return "", "", false
}

// RefreshIgnore rewrites a valid managed ignore block that an earlier release
// wrote (without StagePattern, or compiled for the other core.ignorecase
// setting) as the current block for the same selections, keeping the bytes
// outside it. It reports whether it rewrote the block, and undoes a rewrite
// that fails verification.
func RefreshIgnore(ctx context.Context, r *gitexec.Runner, cwd string) (bool, []string, error) {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return false, nil, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return false, nil, err
	}
	if len(pending) > 0 {
		return false, nil, pendingError(pending[0])
	}
	if outdated, err := s.ignoreOutdated(); err != nil || !outdated {
		return false, nil, err
	}
	current, err := readImage(s.common, "info/exclude")
	if err != nil {
		return false, nil, err
	}
	all, err := states(s.sources, "", nil)
	if err != nil {
		return false, nil, err
	}
	patterns, alternates, err := s.compile(all)
	if err != nil {
		return false, nil, err
	}
	user, err := exclude.UserBytes(current.Data, patterns, alternates...)
	if err != nil {
		return false, nil, err
	}
	mode := os.FileMode(current.Mode)
	if !current.Exists {
		// A repository with no selections from an earlier release, or one
		// created without Git's template, may have no info/exclude yet.
		mode = 0600
		if err := s.common.MkdirAll("info"); err != nil {
			return false, nil, err
		}
	}
	if err := s.common.AtomicWrite("info/exclude", exclude.Render(user, patterns), mode); err != nil {
		return false, nil, err
	}
	warnings, err := s.verify(ctx, "", selection.Empty(), true)
	if err != nil {
		undo := s.common.AtomicWrite("info/exclude", current.Data, mode)
		if !current.Exists {
			undo = s.common.Remove("info/exclude")
		}
		if undo != nil {
			return false, warnings, fmt.Errorf("%w; restoring info/exclude also failed: %v", err, undo)
		}
		return false, warnings, err
	}
	return true, warnings, nil
}

func (s *session) ignoreOutdated() (bool, error) {
	before, err := readImage(s.common, "info/exclude")
	if err != nil {
		return false, err
	}
	all, err := states(s.sources, "", nil)
	if err != nil {
		return false, err
	}
	patterns, alternates, err := s.compile(all)
	if err != nil {
		return false, err
	}
	return exclude.Outdated(before.Data, patterns, alternates...), nil
}
