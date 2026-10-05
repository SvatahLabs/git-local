package selection

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitlocal/internal/storage"
)

type Entry struct {
	Path       string
	Mode       fs.FileMode
	LinkTarget string
}
type Plan struct {
	State       State
	Entries     []Entry
	Directories []string
	Missing     []Rule
	Ownership   Ownership
}

// Engine owns no persistent state and never reads regular-file contents. Domain
// callers must hold the common lock through validation and eventual publication.
// External Git/editor mutations are detected where observable, not coordinated.
type Engine struct {
	git         GitReader
	root, admin string
	files       *storage.Dir
}

func New(g GitReader, root, admin string) (*Engine, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	d, err := storage.OpenDir(root)
	if err != nil {
		return nil, err
	}
	return &Engine{g, root, admin, d}, nil
}
func (e *Engine) Close() error { return e.files.Close() }

// Normalize interprets a literal argument relative to the invocation directory.
// Traversal components are rejected before cleaning, even if they lead back in.
// It is lexical and does not inspect the selected path, allowing safe untracking
// of missing, unsafe or now-main-owned selections.
func (e *Engine) Normalize(cwd, arg string) (string, error) {
	if arg == "" || filepath.IsAbs(arg) || filepath.VolumeName(arg) != "" {
		return "", errors.New("selection arguments must be relative paths inside the checkout")
	}
	arg = filepath.ToSlash(arg)
	for _, part := range strings.Split(arg, "/") {
		if part == ".." {
			return "", errors.New("parent traversal is not allowed")
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(e.root, cwd)
	if err != nil || rel != "." && !filepath.IsLocal(rel) {
		return "", errors.New("invocation directory is outside this checkout")
	}
	path := filepath.ToSlash(filepath.Clean(filepath.Join(rel, filepath.FromSlash(arg))))
	if path == "." {
		return "", errors.New("the checkout root cannot be selected")
	}
	if err = storage.ValidateSelectionPath(path); err != nil {
		return "", err
	}
	return path, nil
}

func (e *Engine) PlanTrack(ctx context.Context, s State, cwd string, args ...string) (Plan, error) {
	if len(args) == 0 {
		return Plan{}, errors.New("at least one path is required")
	}
	original, err := Canonical(s)
	if err != nil {
		return Plan{}, err
	}
	next := original
	for _, arg := range args {
		path, err := e.Normalize(cwd, arg)
		if err != nil {
			return Plan{}, err
		}
		if err = e.boundaries(path); err != nil {
			return Plan{}, err
		}
		fi, err := e.files.Lstat(path)
		if err != nil {
			return Plan{}, fmt.Errorf("inspect selection %q: %w", path, err)
		}
		kind := "file"
		if fi.IsDir() {
			kind = "directory"
		} else if fi.Mode()&os.ModeSymlink == 0 && !fi.Mode().IsRegular() {
			return Plan{}, fmt.Errorf("unsupported selected file type at %q", path)
		}
		// A retained selection cannot silently broaden if its path changed kind.
		for _, r := range original.Include {
			if r.Path == path && r.Kind != kind {
				return Plan{}, fmt.Errorf("selection kind changed at %q; untrack it before re-tracking", path)
			}
		}
		next.Generation = original.Generation
		next, err = next.Track(Rule{path, kind})
		if err != nil {
			return Plan{}, err
		}
	}
	// One command is one generation change, regardless of argument count.
	next.Generation = original.Generation
	next, err = changed(original, next)
	if err != nil {
		return Plan{}, err
	}
	return e.Validate(ctx, next)
}
func (e *Engine) PlanUntrack(s State, cwd string, args ...string) (State, error) {
	if len(args) == 0 {
		return State{}, errors.New("at least one path is required")
	}
	original, err := Canonical(s)
	if err != nil {
		return State{}, err
	}
	next := original
	for _, arg := range args {
		p, err := e.Normalize(cwd, arg)
		if err != nil {
			return State{}, err
		}
		next.Generation = original.Generation
		next, err = next.Untrack(p)
		if err != nil {
			return State{}, err
		}
	}
	next.Generation = original.Generation
	return changed(original, next)
}
func (e *Engine) Validate(ctx context.Context, s State) (Plan, error) {
	n, err := Canonical(s)
	if err != nil {
		return Plan{}, err
	}
	owner, err := InspectOwnership(ctx, e.git, e.root, e.admin)
	if err != nil {
		return Plan{}, err
	}
	if err = owner.Check(n); err != nil {
		return Plan{}, err
	}
	plan := Plan{State: n, Entries: []Entry{}, Missing: []Rule{}, Ownership: owner}
	aliases := map[string]string{}
	for _, r := range n.Include {
		if err = validatePath(r.Path, aliases); err != nil {
			return Plan{}, err
		}
	}
	for _, p := range n.Exclude {
		if err = validatePath(p, aliases); err != nil {
			return Plan{}, err
		}
	}
	for _, r := range n.Include {
		if err = ctx.Err(); err != nil {
			return Plan{}, err
		}
		if err = e.boundaries(r.Path); err != nil {
			return Plan{}, err
		}
		fi, err := e.files.Lstat(r.Path)
		if os.IsNotExist(err) {
			plan.Missing = append(plan.Missing, r)
			continue
		}
		if err != nil {
			return Plan{}, err
		}
		kind := "file"
		if fi.IsDir() {
			kind = "directory"
		}
		if kind != r.Kind {
			return Plan{}, fmt.Errorf("selection kind changed at %q; untrack it before re-tracking", r.Path)
		}
		if err = e.walk(ctx, n, r.Path, aliases, &plan.Entries, &plan.Directories); err != nil {
			return Plan{}, err
		}
	}
	sort.Slice(plan.Entries, func(i, j int) bool { return plan.Entries[i].Path < plan.Entries[j].Path })
	if err = owner.Revalidate(ctx, e.git, e.root, e.admin); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// boundaries inspects ancestors only, never an excluded descendant or file
// contents. This also prevents selecting a file inside a nested repository.
func (e *Engine) boundaries(path string) error {
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		fi, err := e.files.Lstat(parent)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("unsafe non-directory ancestor %q", parent)
		}
		if err = storage.ValidateInfo(fi); err != nil {
			return err
		}
		entries, err := e.files.ReadDir(parent)
		if err != nil {
			return err
		}
		if err = nestedRepository(parent, entries); err != nil {
			return err
		}
	}
	return nil
}
func nestedRepository(path string, entries []os.DirEntry) error {
	found := map[string]bool{}
	for _, entry := range entries {
		key := storage.AliasKey(entry.Name())
		found[key] = true
		if key == ".git" || strings.HasPrefix(key, "git~") {
			return fmt.Errorf("nested repository or Git metadata at %q", path)
		}
	}
	if found["head"] && found["config"] && found["objects"] && found["refs"] {
		return fmt.Errorf("nested bare repository at %q", path)
	}
	return nil
}
func (e *Engine) walk(ctx context.Context, s State, path string, aliases map[string]string, out *[]Entry, directories *[]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Excluded(path) {
		return nil
	}
	if err := validatePath(path, aliases); err != nil {
		return err
	}
	fi, err := e.files.Lstat(path)
	if err != nil {
		return fmt.Errorf("selected path changed or cannot be inspected at %q: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := e.files.ReadLink(path)
		if err != nil {
			return fmt.Errorf("inspect link %q: %w", path, err)
		}
		*out = append(*out, Entry{path, fi.Mode(), target})
		return nil
	}
	if err = storage.ValidateInfo(fi); err != nil {
		return fmt.Errorf("unsafe selected path %q: %w", path, err)
	}
	if fi.IsDir() {
		*directories = append(*directories, path)
		entries, err := e.files.ReadDir(path)
		if err != nil {
			return err
		}
		if err = nestedRepository(path, entries); err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err = e.walk(ctx, s, path+"/"+entry.Name(), aliases, out, directories); err != nil {
				return err
			}
		}
		return nil
	}
	if err = e.files.CheckFile(path); err != nil {
		return err
	}
	*out = append(*out, Entry{Path: path, Mode: fi.Mode()})
	return nil
}
