// Package workspace discovers Git layouts and owns durable workspace identities.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
)

type Context struct {
	Root         string
	Admin        string
	Common       string
	Exclude      string
	Store        string
	RepositoryID string
	WorkspaceID  string
	Head         string
	Branch       string
	// Warnings from Init about registered worktrees it had to skip.
	Warnings []string
}

// StatRoot stats a registered worktree root. Tests replace it to simulate an
// offline volume.
var StatRoot = os.Stat

func pathOutput(b []byte, cwd string) (string, error) {
	s := strings.TrimSuffix(string(b), "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" || strings.ContainsAny(s, "\x00\r\n") {
		return "", errors.New("unsupported Git administrative path")
	}
	if !filepath.IsAbs(s) {
		s = filepath.Join(cwd, s)
	}
	return filepath.Clean(s), nil
}
func Discover(ctx context.Context, r *gitexec.Runner, cwd string) (*Context, error) {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	bare, err := r.Discover(ctx, cwd, "rev-parse", "--is-bare-repository")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(bare)) != "false" {
		return nil, errors.New("working tree required; bare repositories are unsupported")
	}
	w := &Context{}
	queries := []struct {
		target *string
		args   []string
	}{{&w.Root, []string{"--show-toplevel"}}, {&w.Admin, []string{"--absolute-git-dir"}}, {&w.Common, []string{"--path-format=absolute", "--git-common-dir"}}, {&w.Exclude, []string{"--path-format=absolute", "--git-path", "info/exclude"}}}
	for _, q := range queries {
		b, e := r.Discover(ctx, cwd, append([]string{"rev-parse"}, q.args...)...)
		if e != nil {
			return nil, e
		}
		*q.target, e = pathOutput(b, cwd)
		if e != nil {
			return nil, e
		}
	}
	for _, target := range []*string{&w.Root, &w.Admin, &w.Common} {
		*target, err = filepath.EvalSymlinks(*target)
		if err != nil {
			return nil, err
		}
	}
	w.Store = filepath.Join(w.Common, "gitlocal", "history.git")
	if err = validateLayout(ctx, r, w); err != nil {
		return nil, err
	}
	head, err := r.Main(ctx, w.Root, w.Admin, "rev-parse", "--verify", "--quiet", "HEAD")
	if err == nil {
		w.Head = strings.TrimSpace(string(head))
	} else {
		var ge *gitexec.Error
		if !errors.As(err, &ge) || ge.Code != 1 {
			return nil, err
		}
	}
	branch, err := r.Main(ctx, w.Root, w.Admin, "symbolic-ref", "--quiet", "HEAD")
	if err == nil {
		w.Branch = strings.TrimSpace(string(branch))
	} else {
		var ge *gitexec.Error
		if !errors.As(err, &ge) || ge.Code != 1 {
			return nil, err
		}
	}
	return w, nil
}
func contained(root, path string) bool {
	rel, err := filepath.Rel(storage.AliasKey(root), storage.AliasKey(path))
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
func registeredRoots(ctx context.Context, r *gitexec.Runner, w *Context) ([]string, error) {
	b, err := r.Main(ctx, w.Root, w.Admin, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, record := range bytes.Split(b, []byte{0}) {
		if bytes.HasPrefix(record, []byte("worktree ")) {
			p := string(record[len("worktree "):])
			// Any error means the worktree is unavailable: an offline volume
			// reports ESTALE, ENOTCONN, EIO, ENOTDIR or ERROR_NOT_READY, not
			// only ENOENT or EACCES.
			if _, e := StatRoot(p); e == nil {
				if resolved, e := filepath.EvalSymlinks(p); e == nil {
					p = resolved
				}
			}
			if filepath.Clean(p) != w.Common {
				roots = append(roots, filepath.Clean(p))
			}
		}
	}
	found := false
	for _, root := range roots {
		if root == w.Root {
			found = true
		}
	}
	if !found {
		roots = append(roots, w.Root)
	}
	return roots, nil
}
func validateLayout(ctx context.Context, r *gitexec.Runner, w *Context) error {
	roots, err := registeredRoots(ctx, r, w)
	if err != nil {
		return err
	}
	// Git can report the administrative path as the main worktree in separate-
	// git-dir layouts. Also discover enclosing repositories from metadata parents
	// so exposed metadata cannot evade containment checks through that limitation.
	for _, meta := range []string{w.Admin, w.Common} {
		for parent := filepath.Dir(meta); ; parent = filepath.Dir(parent) {
			root, e := r.RootAt(ctx, parent)
			if e == nil {
				roots = append(roots, root)
				break
			}
			var ge *gitexec.Error
			if !errors.As(e, &ge) || !ge.DiscoveryMiss {
				return e
			}
			if parent == filepath.Dir(parent) {
				break
			}
		}
	}
	b, err := r.Main(ctx, w.Root, w.Admin, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return err
	}
	objects, err := pathOutput(b, w.Root)
	if err != nil {
		return err
	}
	objects, err = filepath.EvalSymlinks(objects)
	if err != nil {
		return err
	}
	for _, p := range []string{w.Admin, w.Common, objects} {
		for _, root := range roots {
			if contained(root, p) {
				protected := filepath.Join(root, ".git")
				fi, e := os.Lstat(protected)
				if e != nil || !fi.IsDir() || !contained(protected, p) {
					return fmt.Errorf("exposed Git metadata %s under worktree %s is unsupported", p, root)
				}
			}
		}
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return err
	}
	defer common.Close()
	if fi, e := common.Lstat("gitlocal"); e == nil {
		if !fi.IsDir() {
			return errors.New("extension state must be a real directory")
		}
		if e = storage.ValidateInfo(fi); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return nil
}

// RegisteredRoots lists current registrations; durable workspace records remain
// authoritative for selections when a registered checkout is unavailable.
// The main checkout comes first, unless the repository is bare.
func RegisteredRoots(ctx context.Context, r *gitexec.Runner, w *Context) ([]string, error) {
	return registeredRoots(ctx, r, w)
}

// Bare reports whether the repository is bare, so its worktrees are all
// linked ones and it has no main checkout.
func Bare(ctx context.Context, r *gitexec.Runner, w *Context) (bool, error) {
	b, err := r.Main(ctx, w.Root, w.Admin, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	records := bytes.Split(b, []byte{0})
	return len(records) > 1 && bytes.HasPrefix(records[0], []byte("worktree ")) && string(records[1]) == "bare", nil
}
