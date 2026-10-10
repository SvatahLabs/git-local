package configtx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// OutcomeAbandoned marks a journal that was given up with recover --abandon.
// The journal is archived unchanged otherwise, so its images stay available.
const OutcomeAbandoned = "abandoned"

// Abandon gives up a pending configuration or seed transaction without
// resuming or rolling it back. Selection and destination files stay as they
// are; a configuration transaction's managed ignore block is replaced by the
// one for the live selections. A seed staging file is removed when it matches
// its recorded blob, which local history still holds; any other staging bytes
// are quarantined.
func Abandon(ctx context.Context, r *gitexec.Runner, cwd, id string) (AbandonResult, error) {
	result := AbandonResult{TransactionID: id}
	if !storage.ValidUUID(id) {
		return result, errors.New("invalid transaction ID")
	}
	s, err := open(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return result, err
	}
	for _, other := range pending {
		if other != id {
			return result, pendingError(other)
		}
	}
	if _, err := s.meta.Lstat(seedPath(id)); err == nil {
		result.Kind = "seed"
		return s.abandonSeed(ctx, id, result)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	result.Kind = "config"
	var j Journal
	if err := s.meta.ReadJSON(journalPath(id), &j); errors.Is(err, os.ErrNotExist) {
		return result, abandonedAlready(s.meta, id)
	} else if err != nil {
		return result, err
	}
	if err := validateJournal(j, s.w.RepositoryID, id); err != nil {
		return result, err
	}
	if result.Warnings, result.ExcludeRewritten, err = s.reinstallExclude(j); err != nil {
		return result, err
	}
	j.Outcome = OutcomeAbandoned
	if err := s.writeJournal(j); err != nil {
		return result, err
	}
	return result, s.archive(ctx, j, nil)
}

// reinstallExclude replaces the managed block a stopped transaction left in
// info/exclude, which the next selection change would reject as edited, with
// the block for the live selections. Bytes outside the block are kept. A block
// that matches none the transaction could have written is left with a warning.
// It warns about each pattern the rewrite stops ignoring, and reports whether
// it rewrote the file.
func (s *session) reinstallExclude(j Journal) ([]string, bool, error) {
	after := append([]Source{}, j.Sources...)
	for i := range after {
		if after[i].ID == j.WorkspaceID && j.Kind == "retirement" {
			after[i].Record = j.After
		} else if after[i].ID == j.WorkspaceID {
			after[i].Selection = j.After
		}
	}
	live, err := states(s.sources, "", nil)
	if err != nil {
		return nil, false, err
	}
	before, err := states(j.Sources, "", nil)
	if err != nil {
		return nil, false, err
	}
	next, err := states(after, "", nil)
	if err != nil {
		return nil, false, err
	}
	current, err := readImage(s.common, "info/exclude")
	if err != nil {
		return nil, false, err
	}
	var user, old []byte
	found := !current.Exists
	for _, set := range [][]selection.State{live, before, append(append([]selection.State{}, before...), next...), next, nil} {
		if found {
			break
		}
		patterns, alternates, err := s.compile(set)
		if err != nil {
			return nil, false, err
		}
		if data, e := exclude.UserBytes(current.Data, patterns, alternates...); e == nil {
			user, old, found = data, patterns, true
		}
	}
	if !found {
		return []string{"info/exclude's managed block matches none the transaction wrote and was left as it is; restore the lines between the git-local markers by hand, then run git local doctor"}, false, nil
	}
	patterns, _, err := s.compile(live)
	if err != nil {
		return nil, false, err
	}
	data := exclude.Render(user, patterns)
	if current.Exists && bytes.Equal(current.Data, data) {
		return nil, false, nil
	}
	var warnings []string
	kept := strings.Split(string(patterns), "\n")
	for _, line := range strings.Split(string(old), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "!") && !slices.Contains(kept, line) {
			warnings = append(warnings, fmt.Sprintf("info/exclude no longer ignores %s, which the abandoned transaction's block covered; it isn't selected, so git status may show it", line))
		}
	}
	mode := os.FileMode(current.Mode)
	if !current.Exists {
		mode = 0600
	}
	if err := s.common.MkdirAll("info"); err != nil {
		return warnings, false, err
	}
	return warnings, true, s.common.AtomicWrite("info/exclude", data, mode)
}

func abandonedAlready(meta *storage.Dir, id string) error {
	for _, name := range []string{completedPath(id), seedCompletedPath(id)} {
		var record struct {
			Outcome string `json:"outcome"`
		}
		data, err := meta.ReadFile(name, 16<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.Outcome != OutcomeAbandoned {
			return errors.New("transaction already completed with a different outcome")
		}
		return nil
	}
	return os.ErrNotExist
}

func (s *session) abandonSeed(ctx context.Context, id string, result AbandonResult) (AbandonResult, error) {
	var j seedJournal
	if err := s.meta.ReadJSON(seedPath(id), &j); err != nil {
		return result, err
	}
	if j.ID != id {
		return result, errors.New("seed journal ID mismatch")
	}
	if j.WorkspaceID != s.w.WorkspaceID {
		return result, fmt.Errorf("%w: abandon seed transaction %s from its destination workspace %s", storage.ErrConflict, id, j.WorkspaceID)
	}
	if err := validateSeed(j, s.w.RepositoryID, s.w.WorkspaceID); err != nil {
		return result, err
	}
	key, keyErr := s.seedKey(j)
	if keyErr != nil {
		key = nil
	}
	files, err := storage.OpenDir(s.w.Root)
	if err != nil {
		return result, err
	}
	defer files.Close()
	q, err := NewQuarantine(s.meta, id, "seed")
	if err != nil {
		return result, err
	}
	for _, file := range j.Files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if _, err := files.Lstat(file.Stage); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return result, err
		}
		reason := "staging bytes don't match the recorded seed file"
		if file.PlainDigest == "" || key != nil {
			identity := secureblob.Identity{RepositoryID: s.w.RepositoryID, WorkspaceID: j.Source, Path: file.Path, Mode: file.Mode}
			_, match, err := fileMatches(files, file, file.Stage, key, identity)
			if err != nil {
				return result, err
			}
			if match {
				if err := files.Remove(file.Stage); err != nil {
					return result, err
				}
				result.Removed = append(result.Removed, file.Stage)
				continue
			}
		} else {
			reason = "sensitive-file key is unavailable to verify the staging bytes"
		}
		entry, err := q.QuarantineFile(s.meta, files, file.Stage, s.w.Root, reason)
		if err != nil {
			return result, err
		}
		result.Quarantined = append(result.Quarantined, entry)
	}
	if keyErr != nil && len(result.Quarantined) > 0 {
		result.Warnings = append(result.Warnings, "sensitive-file key unavailable: "+keyErr.Error())
	}
	result.Quarantine = q.Directory(s.w.Common)
	j.Outcome = OutcomeAbandoned
	if err := s.writeSeed(j); err != nil {
		return result, err
	}
	return result, s.archiveSeed(j)
}

// PendingJournal describes an unfinished transaction for doctor and recover.
// Fields are best effort; Err reports a journal that couldn't be read.
type PendingJournal struct {
	ID, Kind     string
	WorkspaceID  string
	Root         string
	Direction    string
	Outcome      string
	Published    bool
	AdminMissing bool
	// MovedTo is the administrative directory that now holds the identity of
	// an initialization journal whose recorded checkout moved.
	MovedTo string
	// IdentityErr is a worktree identity file that blocks checking where an
	// initialization journal's identity was published.
	IdentityErr *workspace.IdentityError
	Err         error
}

// Next returns the action that resolves the journal and the state behind it.
func (p PendingJournal) Next() (string, string) {
	from := ""
	if p.Root != "" && (p.Kind == "seed" || p.Kind == "restore") {
		from = " from " + p.Root
	}
	abandon := "git local recover " + p.ID + " --abandon" + from
	switch {
	case p.Err != nil:
		return "Inspect the transactions directory; don't delete journals blindly.", "journal can't be read: " + p.Err.Error()
	case p.IdentityErr != nil:
		return "Repair " + p.IdentityErr.Path + " or move it aside if that worktree is gone, then run git local doctor again.", "initialization of " + p.Root + " can't be checked: " + p.IdentityErr.Error()
	case p.Kind == "init" && p.MovedTo != "":
		return "Run git local init in the checkout whose Git directory is " + p.MovedTo + ".", "initialization of " + p.Root + " never finished and the checkout has moved"
	case p.Kind == "init" && p.AdminMissing:
		return "Run " + abandon + ".", "initialization of " + p.Root + " never finished and its Git administrative directory no longer exists"
	case p.Kind == "init":
		return "Run git local init in " + p.Root + ".", "initialization of " + p.Root + " never finished"
	case p.Kind == "encrypt" && p.Published:
		return "Run git local encrypt recover " + p.ID + " --resume. If it fails, run " + abandon + ".", "the encryption policy is published and only cleanup remains"
	case p.Kind == "encrypt":
		return "Run git local encrypt recover " + p.ID + " --resume or --rollback. If both fail, run " + abandon + ".", "encryption migration stopped before its policy was published"
	case p.Outcome == "committed":
		return "Run git local recover " + p.ID + " --resume" + from + ".", "the transaction committed and only bookkeeping remains"
	case p.Outcome == "rolledback" || p.Direction == "rollback":
		return "Run git local recover " + p.ID + " --rollback" + from + ". If it fails, run " + abandon + ".", "rollback has started and must finish"
	default:
		return "Run git local recover " + p.ID + " --resume or --rollback" + from + ". If both fail, run " + abandon + ".", "the transaction stopped before it finished"
	}
}

// PendingJournals lists unfinished journals under a shared lock. It needs only
// the lock file, so it also works while an initialization or encryption
// migration journal blocks every other command.
func PendingJournals(ctx context.Context, r *gitexec.Runner, cwd string) ([]PendingJournal, error) {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return nil, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	if _, err := common.Lstat("gitlocal"); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	entries, err := common.ReadDir("gitlocal/transactions")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	roots := map[string]string{}
	root := func(id string) string {
		if id == "" {
			return ""
		}
		if value, ok := roots[id]; ok {
			return value
		}
		var record workspace.Record
		if common.ReadJSON(filepath.Join("gitlocal/workspaces", id, "workspace.json"), &record) == nil {
			roots[id] = record.Root
		}
		return roots[id]
	}
	var out []PendingJournal
	for _, entry := range entries {
		name := entry.Name()
		if name == "completed" && entry.IsDir() || storage.IsTempName(name) && entry.Type().IsRegular() {
			continue
		}
		data, readErr := common.ReadFile(filepath.Join("gitlocal/transactions", name), 16<<20)
		var record struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspaceId"`
			Direction   string `json:"direction"`
			Outcome     string `json:"outcome"`
			Identity    struct {
				WorkspaceID string `json:"workspaceId"`
			} `json:"identity"`
			Workspace struct {
				Admin string `json:"adminPath"`
				Root  string `json:"rootPath"`
			} `json:"workspace"`
			Manifest struct {
				Plan struct {
					Destination string `json:"destinationWorkspaceId"`
				} `json:"plan"`
			} `json:"manifest"`
			Policy    secureblob.Policy  `json:"policy"`
			OldPolicy *secureblob.Policy `json:"oldPolicy"`
		}
		if readErr == nil {
			readErr = json.NewDecoder(bytes.NewReader(data)).Decode(&record)
		}
		if name == "init.json" {
			item := PendingJournal{ID: record.Identity.WorkspaceID, Kind: "init", WorkspaceID: record.Identity.WorkspaceID, Root: record.Workspace.Root, Err: readErr}
			if readErr == nil {
				if !filepath.IsAbs(record.Workspace.Admin) {
					item.Err = errors.New("initialization journal has no administrative path")
				} else if _, e := os.Stat(record.Workspace.Admin); errors.Is(e, os.ErrNotExist) {
					var identityErr *workspace.IdentityError
					if admin, e := workspace.PublishedIdentityAdmin(w.Common, item.ID); errors.As(e, &identityErr) {
						item.IdentityErr = identityErr
					} else if e != nil {
						item.Err = e
					} else if admin != "" {
						item.MovedTo = admin
					} else {
						item.AdminMissing = true
					}
				}
			}
			out = append(out, item)
			continue
		}
		kind, id, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
		if !strings.HasSuffix(name, ".json") || !ok || !storage.ValidUUID(id) || kind != "config" && kind != "seed" && kind != "restore" && kind != "encrypt" {
			out = append(out, PendingJournal{ID: name, Kind: "unknown", Err: errors.New("unknown transaction file")})
			continue
		}
		item := PendingJournal{ID: id, Kind: kind, WorkspaceID: record.WorkspaceID, Direction: record.Direction, Outcome: record.Outcome, Err: readErr}
		if kind == "restore" {
			item.WorkspaceID = record.Manifest.Plan.Destination
		}
		if kind == "encrypt" && readErr == nil {
			// A correction replaces a policy that is already in service.
			item.Published, item.Err = secureblob.Published(common, record.Policy, record.OldPolicy)
		}
		item.Root = root(item.WorkspaceID)
		out = append(out, item)
	}
	return out, nil
}

// JournalKind finds the kind of a pending or completed journal without
// opening a session, which an encryption or initialization journal blocks.
// An initialization journal is named by its workspace ID.
func JournalKind(ctx context.Context, r *gitexec.Runner, cwd, id string) (string, error) {
	pending, err := PendingJournals(ctx, r, cwd)
	if err != nil {
		return "", err
	}
	for _, p := range pending {
		if p.ID == id {
			return p.Kind, nil
		}
	}
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return "", err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return "", err
	}
	defer common.Close()
	for _, kind := range []string{"config", "seed", "restore", "encrypt", "init"} {
		if _, err := common.Lstat("gitlocal/transactions/completed/" + kind + "-" + id + ".json"); err == nil {
			return kind, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", os.ErrNotExist
}
