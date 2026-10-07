package configtx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type WorkspaceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Root       string `json:"root"`
	LastRoot   string `json:"lastRoot,omitempty"`
	Admin      string `json:"admin,omitempty"`
	Registered bool   `json:"registered"`
	Available  bool   `json:"available"`
	Locked     bool   `json:"locked"`
	Moved      bool   `json:"moved"`
}
type registration struct {
	root, admin, id string
	locked          bool
}

func cleanRoot(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(p)
}
func (s *session) registrations(ctx context.Context) ([]registration, error) {
	data, err := s.r.Main(ctx, s.w.Root, s.w.Admin, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	regs := []registration{}
	for _, field := range bytes.Split(data, []byte{0}) {
		value := string(field)
		if strings.HasPrefix(value, "worktree ") {
			regs = append(regs, registration{root: cleanRoot(strings.TrimPrefix(value, "worktree "))})
		} else if value == "locked" || strings.HasPrefix(value, "locked ") {
			if len(regs) == 0 {
				return nil, errors.New("malformed worktree registration")
			}
			regs[len(regs)-1].locked = true
		}
	}
	if len(regs) == 0 {
		return nil, errors.New("missing Git worktree registrations")
	}
	// Administrative directories remain available when a worktree volume is offline.
	admins := map[string]string{s.w.Common: regs[0].root}
	if s.w.Admin == s.w.Common {
		admins[s.w.Common] = s.w.Root
		regs[0].root = s.w.Root
	}
	entries, err := s.common.ReadDir("worktrees")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf("%w: unsafe worktree administrative directory", storage.ErrConflict)
		}
		rel := filepath.Join("worktrees", entry.Name())
		b, e := s.common.ReadFile(filepath.Join(rel, "gitdir"), 64<<10)
		if e != nil {
			return nil, e
		}
		path := strings.TrimSuffix(string(b), "\n")
		path = strings.TrimSuffix(path, "\r")
		if path == "" || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("unsupported worktree gitdir pointer")
		}
		if !filepath.IsAbs(path) {
			// Git 2.48+ records it relative to the administrative directory
			// with worktree.useRelativePaths or --relative-paths.
			path = filepath.Join(s.w.Common, rel, filepath.FromSlash(path))
		}
		admins[filepath.Join(s.w.Common, rel)] = cleanRoot(filepath.Dir(path))
	}
	seen := map[string]string{}
	for admin, root := range admins {
		dir, e := storage.OpenDir(admin)
		if e != nil {
			return nil, e
		}
		var identity workspace.Identity
		e = dir.ReadJSON("gitlocal-workspace.json", &identity)
		dir.Close()
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		id := ""
		if e == nil {
			if identity.SchemaVersion != workspace.SchemaVersion || identity.RepositoryID != s.w.RepositoryID || !storage.ValidUUID(identity.WorkspaceID) {
				return nil, fmt.Errorf("%w: invalid registered workspace identity", storage.ErrConflict)
			}
			id = identity.WorkspaceID
			if other, ok := seen[id]; ok && other != admin {
				return nil, fmt.Errorf("%w: duplicate workspace identity %s", storage.ErrConflict, id)
			}
			seen[id] = admin
		}
		matched := false
		for i := range regs {
			if regs[i].root == root {
				if regs[i].admin != "" && regs[i].admin != admin {
					return nil, fmt.Errorf("%w: ambiguous worktree location", storage.ErrConflict)
				}
				regs[i].admin = admin
				regs[i].id = id
				matched = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("%w: worktree registration/admin mapping is inconsistent; repair or prune with Git first", storage.ErrConflict)
		}
	}
	return regs, nil
}
func (s *session) inventory(ctx context.Context) ([]WorkspaceInfo, error) {
	regs, err := s.registrations(ctx)
	if err != nil {
		return nil, err
	}
	out := []WorkspaceInfo{}
	for _, src := range s.sources {
		record, err := src.record()
		if err != nil {
			return nil, err
		}
		item := WorkspaceInfo{ID: src.ID, State: record.State, Root: record.Root, LastRoot: record.Root, Admin: record.Admin}
		for _, reg := range regs {
			matches := reg.id == src.ID || reg.id == "" && (reg.admin == record.Admin || reg.root == cleanRoot(record.Root))
			if !matches {
				continue
			}
			if item.Registered {
				return nil, fmt.Errorf("%w: ambiguous workspace registration", storage.ErrConflict)
			}
			item.Registered = true
			item.Root = reg.root
			item.Admin = reg.admin
			item.Locked = reg.locked
			item.Moved = cleanRoot(record.Root) != reg.root
			if fi, e := os.Stat(reg.root); e == nil && fi.IsDir() {
				admin, e := s.r.AdminAt(ctx, reg.root)
				if e == nil && cleanRoot(admin) == cleanRoot(reg.admin) && reg.id == src.ID {
					item.Available = true
				}
			}
		}
		if record.State == "retained" {
			if item.Registered {
				return nil, fmt.Errorf("%w: retained workspace %s is registered again", storage.ErrConflict, src.ID)
			}
		} else if !item.Registered {
			item.State = "missing"
		} else if !item.Available {
			item.State = "unavailable"
		} else if item.Moved {
			item.State = "moved"
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func ListWorkspaces(ctx context.Context, r *gitexec.Runner, cwd string) ([]WorkspaceInfo, error) {
	s, err := openMode(ctx, r, cwd, true)
	if err != nil {
		return nil, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, pendingError(pending[0])
	}
	return s.inventory(ctx)
}
func (s *session) retireEligible(ctx context.Context, id string) error {
	regs, err := s.registrations(ctx)
	if err != nil {
		return err
	}
	for _, reg := range regs {
		if reg.id == id {
			return fmt.Errorf("%w: workspace %s is still registered with Git (including locked or unavailable worktrees)", storage.ErrConflict, id)
		}
		if reg.id == "" {
			return fmt.Errorf("%w: an unidentified Git worktree prevents proving retirement eligibility; initialize or reconcile it first", storage.ErrConflict)
		}
	}
	return nil
}

// RequireWorkspace validates an explicit source against this repository's registry.
// Call only inside a locked ReadSelection/WithSelection/ChangePlanned callback.
func RequireWorkspace(w workspace.Context, id string) error {
	if !storage.ValidUUID(id) {
		return errors.New("invalid workspace UUID")
	}
	d, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		return err
	}
	defer d.Close()
	var record workspace.Record
	if err = d.ReadJSON(filepath.Join("workspaces", id, "workspace.json"), &record); err != nil {
		return err
	}
	if record.SchemaVersion != workspace.SchemaVersion || record.WorkspaceID != id || (record.State != "active" && record.State != "retained") {
		return errors.New("invalid source workspace record")
	}
	return nil
}
