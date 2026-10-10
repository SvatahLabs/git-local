package selection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
func (o Ownership) Revalidate(ctx context.Context, g GitReader, root, admin string) error {
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
		return fmt.Errorf("%w: main HEAD or index changed; retry after Git operations finish", storage.ErrConflict)
	}
	return nil
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
		key := storage.AliasKey(path)
		if folded.Selected(key) {
			return fmt.Errorf("%w: selected path %q is owned by the main index or HEAD; untrack or exclude it", storage.ErrConflict, path)
		}
		for _, r := range folded.Include {
			if within(key, r.Path) {
				return fmt.Errorf("%w: main-owned ancestor %q blocks selection", storage.ErrConflict, path)
			}
		}
	}
	return nil
}
