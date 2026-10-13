package selection

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MainState is what can change main ownership and is cheap to read: the files
// Git replaces to move HEAD (the worktree's HEAD file, the loose ref it names,
// packed-refs and the reftable stack's tables.list) and the index file, by
// identity, size, mode and times. Reading it starts no Git process, where
// Revalidate lists the whole index; a long operation reads it before each
// step and revalidates ownership only when it changed.
type MainState struct {
	// Head is the content of the worktree's HEAD file.
	Head string
	// Files are the stats of the HEAD file, the ref it names, packed-refs and
	// tables.list, in that order; nil when absent.
	Files []fs.FileInfo
	Index fs.FileInfo // nil when there is no index file
	At    time.Time   // when it was read
	// unknown marks a HEAD this reading can't follow (a symbolic ref chain
	// or an unexpected form); such a state is never the same as another.
	unknown bool
}

// IndexFile returns the main index file's path.
func IndexFile(ctx context.Context, g GitReader, root, admin string) (string, error) {
	b, err := g.Main(ctx, root, admin, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", err
	}
	index := strings.TrimSpace(string(b))
	if !filepath.IsAbs(index) {
		index = filepath.Join(root, index)
	}
	return index, nil
}

// worktreeRefs are the ref namespaces Git keeps per worktree, in its
// administrative directory rather than the common one.
var worktreeRefs = []string{"refs/worktree/", "refs/bisect/", "refs/rewritten/"}

// ReadMainState reads the main state of the worktree whose administrative
// directory is admin, in the repository whose common Git directory is common;
// index is the path IndexFile returned. It is read before the checks it
// stamps, so a change made during them shows at the next reading.
func ReadMainState(admin, common, index string) (MainState, error) {
	state := MainState{At: time.Now()}
	head, err := os.ReadFile(filepath.Join(admin, "HEAD"))
	if err != nil {
		return state, err
	}
	state.Head = string(head)
	names := []string{filepath.Join(admin, "HEAD"), "", filepath.Join(common, "packed-refs"), filepath.Join(common, "reftable", "tables.list")}
	if ref, ok := strings.CutPrefix(strings.TrimSuffix(state.Head, "\n"), "ref: "); ok {
		if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") || strings.ContainsAny(ref, "\\\n") {
			state.unknown = true
		}
		dir := common
		for _, prefix := range worktreeRefs {
			if strings.HasPrefix(ref, prefix) {
				dir = admin
			}
		}
		names[1] = filepath.Join(dir, filepath.FromSlash(ref))
	}
	for _, name := range names {
		var info fs.FileInfo
		if name != "" {
			var regular bool
			if info, regular, err = lstatRegular(name); os.IsNotExist(err) {
				info, err = nil, nil
			} else if err != nil {
				return state, err
			} else if !regular {
				// A symbolic ref stored as a symlink, or anything else
				// unexpected: always revalidate.
				state.unknown = true
			}
		}
		state.Files = append(state.Files, info)
	}
	state.Index, err = StatIndex(index)
	if os.IsNotExist(err) {
		state.Index, err = nil, nil
	}
	return state, err
}

// Same reports whether nothing that can change ownership changed between two
// readings: the same HEAD content, and the same HEAD, ref and index files.
func (s MainState) Same(o MainState) bool {
	if s.unknown || o.unknown || s.Head != o.Head || len(s.Files) != len(o.Files) {
		return false
	}
	for i := range s.Files {
		if !sameOptional(s.Files[i], o.Files[i]) {
			return false
		}
	}
	return sameOptional(s.Index, o.Index)
}

func sameOptional(a, b fs.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return SameIndexFile(a, b)
}

// SameIndexFile compares two stats of a file Git replaces to change it:
// identity, size, mode and times.
func SameIndexFile(a, b fs.FileInfo) bool {
	return SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
}

// Settled reports whether a later reading would show a change of the files:
// false while a timestamp is too close to when it was read (see Racy). A
// state that isn't settled mustn't stand for a check.
func (s MainState) Settled() bool {
	if s.unknown {
		return false
	}
	for _, info := range append(append([]fs.FileInfo{}, s.Files...), s.Index) {
		if info != nil && Racy(info, s.At) {
			return false
		}
	}
	return true
}

// Racy reports whether a file's timestamps are too close to when it was read
// or hashed for a later Lstat to show a change, as Git's racy-clean rule: a
// same-size rewrite within the same timestamp tick would leave every field
// equal. A timestamp with no sub-second part may come from a filesystem with
// up to two-second granularity (HFS+, FAT).
func Racy(info fs.FileInfo, hashed time.Time) bool {
	for _, t := range []time.Time{info.ModTime(), ChangeTime(info)} {
		if t.IsZero() {
			continue
		}
		limit := hashed
		if t.Nanosecond() == 0 {
			limit = hashed.Add(-2 * time.Second)
		}
		if !t.Before(limit) {
			return true
		}
	}
	return false
}
