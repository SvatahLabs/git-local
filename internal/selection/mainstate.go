package selection

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// MainState is what can change main ownership and is cheap to read: the files
// Git replaces to move HEAD (the worktree's HEAD file, the loose ref it names,
// packed-refs and the reftable stacks' tables.list), the configuration files
// that choose the ref backend, and the index file, by identity, size, mode and
// times. Reading it starts no Git process, where
// Revalidate lists the whole index; a long operation reads it before each
// step and revalidates ownership only when it changed.
type MainState struct {
	// Head is the content of the worktree's HEAD file.
	Head string
	// Files are the stats of the HEAD file, the ref it names, packed-refs,
	// the repository's tables.list, for a linked worktree its own
	// tables.list, the repository's config and the worktree's
	// config.worktree, in that order; nil when absent.
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
	// The reftable backend keeps a linked worktree's HEAD in the worktree's
	// own stack.
	if filepath.Clean(admin) != filepath.Clean(common) {
		names = append(names, filepath.Join(admin, "reftable", "tables.list"))
	}
	names = append(names, filepath.Join(common, "config"), filepath.Join(admin, "config.worktree"))
	content := strings.TrimSuffix(state.Head, "\n")
	if ref, ok := strings.CutPrefix(content, "ref: "); ok && strings.HasPrefix(ref, "refs/") && !strings.Contains(ref, "..") && !strings.ContainsAny(ref, "\\ \t\r\n") {
		dir := common
		for _, prefix := range worktreeRefs {
			if strings.HasPrefix(ref, prefix) {
				dir = admin
			}
		}
		names[1] = filepath.Join(dir, filepath.FromSlash(ref))
	} else if len(content) != 40 && len(content) != 64 || !objectID(content) {
		// Git also accepts other spellings ("ref:" without a space,
		// trailing whitespace); always revalidate rather than guess.
		state.unknown = true
	}
	files, unknown, err := statRefFiles(names, func(i int) bool { return i == 1 })
	state.Files, state.unknown = files, state.unknown || unknown
	if err != nil {
		return state, err
	}
	state.Index, err = StatIndex(index)
	if os.IsNotExist(err) {
		state.Index, err = nil, nil
	}
	return state, err
}

// statRefFiles stats the files Git replaces to change refs; an empty or
// absent name gives nil. loose reports which names are loose refs, which hold
// an object name; unknown reports a file this reading doesn't follow.
func statRefFiles(names []string, loose func(int) bool) ([]fs.FileInfo, bool, error) {
	files, unknown := make([]fs.FileInfo, 0, len(names)), false
	for i, name := range names {
		var info fs.FileInfo
		if name != "" {
			var regular bool
			var err error
			info, regular, err = lstatRegular(name)
			switch {
			case os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR):
				// Absent. With the reftable backend refs/heads is a file,
				// so a loose ref under it is "not a directory".
				info = nil
			case err != nil:
				return files, unknown, err
			case !regular:
				// A symbolic ref stored as a symlink, or anything else
				// unexpected: always revalidate.
				unknown = true
			case loose(i) && symbolic(name):
				// The ref names another ref (a symbolic ref chain), which
				// this reading doesn't follow.
				unknown = true
			}
		}
		files = append(files, info)
	}
	return files, unknown, nil
}

// symbolic reports whether the loose ref file name holds a symbolic ref (or
// can't be read). A loose ref is an object name and a newline.
func symbolic(name string) bool {
	f, err := os.Open(name)
	if err != nil {
		return true
	}
	defer f.Close()
	b := make([]byte, 4)
	n, _ := io.ReadFull(f, b)
	return string(b[:n]) == "ref:"
}

// Same reports whether nothing that can change ownership changed between two
// readings: the same HEAD content, and the same HEAD, ref, config and index
// files.
func (s MainState) Same(o MainState) bool {
	return !s.unknown && !o.unknown && s.Head == o.Head && sameFiles(s.Files, o.Files) && sameOptional(s.Index, o.Index)
}

func sameFiles(a, b []fs.FileInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameOptional(a[i], b[i]) {
			return false
		}
	}
	return true
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
	return !s.unknown && settledFiles(append(append([]fs.FileInfo{}, s.Files...), s.Index), s.At)
}

func settledFiles(files []fs.FileInfo, at time.Time) bool {
	for _, info := range files {
		if info != nil && Racy(info, at) {
			return false
		}
	}
	return true
}

// RefFiles is what can change some refs of a Git directory no worktree
// checks out, such as the private store, read by stat as MainState reads
// HEAD: each loose ref, packed-refs, the reftable stack's tables.list and the
// config. Reading it starts no Git process; a long operation reads the refs
// with Git again only when it changed or isn't settled.
type RefFiles struct {
	Files   []fs.FileInfo // nil when absent
	At      time.Time     // when it was read
	unknown bool
}

// ReadRefFiles reads the files that can change refs (full ref names) in the
// Git directory dir. It is read before the checks it stamps.
func ReadRefFiles(dir string, refs ...string) (RefFiles, error) {
	state := RefFiles{At: time.Now()}
	names := make([]string, 0, len(refs)+3)
	for _, ref := range refs {
		names = append(names, filepath.Join(dir, filepath.FromSlash(ref)))
	}
	names = append(names, filepath.Join(dir, "packed-refs"), filepath.Join(dir, "reftable", "tables.list"), filepath.Join(dir, "config"))
	var err error
	state.Files, state.unknown, err = statRefFiles(names, func(i int) bool { return i < len(refs) })
	return state, err
}

// Same reports whether no file changed between two readings.
func (s RefFiles) Same(o RefFiles) bool {
	return !s.unknown && !o.unknown && sameFiles(s.Files, o.Files)
}

// Settled reports whether a later reading would show a change (see Racy).
func (s RefFiles) Settled() bool { return !s.unknown && settledFiles(s.Files, s.At) }

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
