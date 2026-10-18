package selection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
)

type GitReader interface {
	Main(context.Context, string, string, ...string) ([]byte, error)
	// MainListing is Main without the output cap, for index and tree
	// listings that grow with the repository.
	MainListing(context.Context, string, string, ...string) ([]byte, error)
}
type Evidence struct {
	Head        string
	IndexDigest string
}
type Ownership struct {
	Paths    []string
	Evidence Evidence
}
type gitEntry struct{ path, mode, oid string }

func parseEntries(data []byte, index bool) ([]gitEntry, error) {
	if len(data) > 0 && data[len(data)-1] != 0 {
		return nil, errors.New("unterminated Git ownership output")
	}
	entries := []gitEntry{}
	for _, line := range bytes.Split(data, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		header, path, ok := strings.Cut(string(line), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, errors.New("malformed Git ownership record")
		}
		mode := fields[0]
		oid := fields[2]
		if index {
			oid = fields[1]
			if fields[2] != "0" && fields[2] != "1" && fields[2] != "2" && fields[2] != "3" {
				return nil, errors.New("unsupported index stage")
			}
		}
		switch mode {
		case "100644", "100755", "120000", "160000", "040000":
		default:
			return nil, errors.New("unsupported Git entry mode")
		}
		if !index {
			want := "blob"
			if mode == "160000" {
				want = "commit"
			}
			if mode == "040000" {
				want = "tree"
			}
			if fields[1] != want {
				return nil, errors.New("inconsistent Git entry type")
			}
		}
		if !objectID(oid) {
			return nil, errors.New("invalid Git ownership object ID")
		}
		if mode == "040000" {
			path = strings.TrimSuffix(path, "/")
		}
		if path == "" || strings.HasPrefix(path, "/") {
			return nil, errors.New("invalid Git ownership path")
		}
		for _, p := range strings.Split(path, "/") {
			if p == "" || p == "." || p == ".." {
				return nil, errors.New("noncanonical Git ownership path")
			}
		}
		entries = append(entries, gitEntry{path, mode, oid})
	}
	return entries, nil
}
func objectID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func head(ctx context.Context, g GitReader, root, admin string) (string, error) {
	b, err := g.Main(ctx, root, admin, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		var e *gitexec.Error
		if errors.As(err, &e) && e.Code == 1 {
			symbolic, symErr := g.Main(ctx, root, admin, "symbolic-ref", "--quiet", "HEAD")
			if symErr != nil {
				return "", fmt.Errorf("cannot establish HEAD ownership: %w", symErr)
			}
			ref := strings.TrimSpace(string(symbolic))
			if !strings.HasPrefix(ref, "refs/") {
				return "", errors.New("invalid symbolic HEAD")
			}
			_, refErr := g.Main(ctx, root, admin, "show-ref", "--verify", "--quiet", ref)
			var missing *gitexec.Error
			if errors.As(refErr, &missing) && missing.Code == 1 {
				return "", nil
			}
			if refErr != nil {
				return "", fmt.Errorf("cannot establish HEAD ownership: %w", refErr)
			}
			return "", fmt.Errorf("%w: HEAD resolution changed or is corrupt", storage.ErrConflict)
		}
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if !objectID(id) {
		return "", errors.New("invalid HEAD object ID")
	}
	return id, nil
}
func indexBytes(ctx context.Context, g GitReader, root, admin string) ([]byte, error) {
	return g.MainListing(ctx, root, admin, "ls-files", "--stage", "--sparse", "--full-name", "-z")
}

// InspectOwnership never interprets Git failures or unavailable trees as empty
// ownership. All index stages are retained. Sparse directory entries are requested
// explicitly and expanded by immutable object ID, making missing trees fatal.
func InspectOwnership(ctx context.Context, g GitReader, root, admin string) (Ownership, error) {
	firstHead, err := head(ctx, g, root, admin)
	if err != nil {
		return Ownership{}, err
	}
	index, err := indexBytes(ctx, g, root, admin)
	if err != nil {
		return Ownership{}, err
	}
	entries, err := parseEntries(index, true)
	if err != nil {
		return Ownership{}, err
	}
	paths := map[string]bool{}
	for _, e := range entries {
		if e.mode == "040000" {
			tree, err := g.MainListing(ctx, root, admin, "ls-tree", "-r", "--full-tree", "-z", e.oid)
			if err != nil {
				return Ownership{}, err
			}
			children, err := parseEntries(tree, false)
			if err != nil {
				return Ownership{}, err
			}
			for _, c := range children {
				paths[e.path+"/"+c.path] = true
			}
		} else {
			paths[e.path] = true
		}
	}
	if firstHead != "" {
		tree, err := g.MainListing(ctx, root, admin, "ls-tree", "-r", "--full-tree", "-z", firstHead)
		if err != nil {
			return Ownership{}, err
		}
		entries, err := parseEntries(tree, false)
		if err != nil {
			return Ownership{}, err
		}
		for _, e := range entries {
			paths[e.path] = true
		}
	}
	digest := sha256.Sum256(index)
	o := Ownership{Evidence: Evidence{firstHead, hex.EncodeToString(digest[:])}}
	for p := range paths {
		o.Paths = append(o.Paths, p)
	}
	sort.Strings(o.Paths)
	if err = o.Revalidate(ctx, g, root, admin); err != nil {
		return Ownership{}, err
	}
	return o, nil
}

// Revalidate checks that main's HEAD and index still match o's evidence,
// which the same command captured moments earlier: a mismatch means a Git
// command ran meanwhile, so running the command again is the fix.
func (o Ownership) Revalidate(ctx context.Context, g GitReader, root, admin string) error {
	return o.revalidate(ctx, g, root, admin, false)
}

// RevalidateRecorded is Revalidate for evidence a journal records, as a seed
// does: a mismatch is usually a lasting change since then, which the
// journal's recovery resolves, so it says to retry only while a Git command
// holds the index or HEAD.
func (o Ownership) RevalidateRecorded(ctx context.Context, g GitReader, root, admin string) error {
	return o.revalidate(ctx, g, root, admin, true)
}

func (o Ownership) revalidate(ctx context.Context, g GitReader, root, admin string, recorded bool) error {
	h, err := head(ctx, g, root, admin)
	if err != nil {
		return err
	}
	b, err := indexBytes(ctx, g, root, admin)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if h != o.Evidence.Head || hex.EncodeToString(sum[:]) != o.Evidence.IndexDigest {
		// Waiting helps only while a Git command holds the index or HEAD.
		if !recorded || gitBusy(admin) {
			return fmt.Errorf("%w: main HEAD or index changed; retry after Git operations finish", storage.ErrConflict)
		}
		return fmt.Errorf("%w: main HEAD or index changed", storage.ErrConflict)
	}
	return nil
}

// gitBusy reports whether a Git command is changing the index or HEAD of the
// worktree whose administrative directory is admin: it holds their lock
// files while it does.
func gitBusy(admin string) bool {
	for _, name := range []string{"index.lock", "HEAD.lock"} {
		if _, err := os.Lstat(filepath.Join(admin, name)); err == nil {
			return true
		}
	}
	return false
}

// Check compares aliases conservatively, while explicit exclusions can remove
// owned descendants from the effective directory selection. Ownership of an
// ancestor (including a gitlink) still blocks every selected descendant.
func (o Ownership) Check(s State) error {
	normalized, err := Canonical(s)
	if err != nil {
		return err
	}
	folded := normalized
	folded.Include = make([]Rule, len(normalized.Include))
	folded.Exclude = make([]string, len(normalized.Exclude))
	for i, r := range normalized.Include {
		folded.Include[i] = Rule{storage.AliasKey(r.Path), r.Kind}
	}
	for i, p := range normalized.Exclude {
		folded.Exclude[i] = storage.AliasKey(p)
	}
	for _, path := range o.Paths {
		key := FoldOwned(path)
		if folded.Selected(key) {
			return ownedError(path)
		}
		for _, r := range folded.Include {
			if within(key, r.Path) {
				return ownedAncestorError(path)
			}
		}
	}
	return nil
}

// FoldOwned is storage.AliasKey for ownership checks; tests replace it to
// count the folds.
var FoldOwned = storage.AliasKey

// OwnedError is the refusal of a selected path main owns. Its message
// suggests untracking or excluding the path; Fact is the message without
// that advice, for a caller (a pending seed's recovery) where neither works.
type OwnedError struct{ Path string }

func (e *OwnedError) Error() string { return e.Fact() + "; untrack or exclude it" }

// Fact states the conflict without advice.
func (e *OwnedError) Fact() string {
	return fmt.Sprintf("%v: selected path %q is owned by the main index or HEAD", storage.ErrConflict, e.Path)
}

func (e *OwnedError) Unwrap() error { return storage.ErrConflict }

func ownedError(path string) error {
	return &OwnedError{Path: path}
}
func ownedAncestorError(path string) error {
	return fmt.Errorf("%w: main-owned ancestor %q blocks selection", storage.ErrConflict, path)
}

// CheckPaths is Check of a directory rule for each path, as restore and seed
// check the paths they write: a path is refused when its alias key, or an
// ancestor's, is main-owned, or when a main-owned key lies under it. It folds
// every owned path once, so checking N paths against M owned paths costs
// about N + M folds rather than N·M. The error names the same owned path
// Check would.
func (o Ownership) CheckPaths(paths []string) error {
	owned := map[string][]int{}
	for i, p := range o.Paths {
		key := FoldOwned(p)
		owned[key] = append(owned[key], i)
	}
	keys := make([]string, 0, len(owned))
	for key := range owned {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, p := range paths {
		s := Empty()
		s.Include = []Rule{{Path: p, Kind: "directory"}}
		normalized, err := Canonical(s)
		if err != nil {
			return err
		}
		rule := FoldOwned(normalized.Include[0].Path)
		// Check reports the first owned path, in o.Paths order, that the rule
		// selects or that is an ancestor of the rule.
		first, ancestor := -1, false
		consider := func(indexes []int, isAncestor bool) {
			for _, i := range indexes {
				if first < 0 || i < first {
					first, ancestor = i, isAncestor
				}
			}
		}
		consider(owned[rule], false)
		for i := sort.SearchStrings(keys, rule+"/"); i < len(keys) && strings.HasPrefix(keys[i], rule+"/"); i++ {
			consider(owned[keys[i]], false)
		}
		for i := 0; i < len(rule); i++ {
			if rule[i] == '/' {
				consider(owned[rule[:i]], true)
			}
		}
		if first >= 0 && ancestor {
			return ownedAncestorError(o.Paths[first])
		}
		if first >= 0 {
			return ownedError(o.Paths[first])
		}
	}
	return nil
}
