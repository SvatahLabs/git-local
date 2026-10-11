package gitexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TemporaryHistory is writable only in its disposable directory. Its alternate
// is supplied as a command-scoped environment value, never installed durably.
type TemporaryHistory struct {
	r                  *Runner
	Path, source, base string
}

func (r *Runner) TemporaryHistory(ctx context.Context, source string) (*TemporaryHistory, error) {
	if err := r.checkStore(source); err != nil {
		return nil, err
	}
	base, err := os.MkdirTemp(r.private, "diff-")
	if err != nil {
		return nil, err
	}
	t := &TemporaryHistory{r: r, Path: filepath.Join(base, "history.git"), source: source, base: base}
	if err = r.InitBare(ctx, t.Path); err != nil {
		os.RemoveAll(base)
		return nil, err
	}
	return t, nil
}
func (t *TemporaryHistory) Close() error { return os.RemoveAll(t.base) }
func alternate(path string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, c := range []byte(path) {
		switch {
		case c == '"' || c == '\\':
			out.WriteByte('\\')
			out.WriteByte(c)
		case c < 32 || c == 127:
			fmt.Fprintf(&out, "\\%03o", c)
		default:
			out.WriteByte(c)
		}
	}
	out.WriteByte('"')
	return out.String()
}
func (t *TemporaryHistory) History(ctx context.Context, store string, input io.Reader, args ...string) ([]byte, error) {
	if store != t.Path || len(args) == 0 {
		return nil, errors.New("invalid temporary history operation")
	}
	switch args[0] {
	case "hash-object", "mktree", "diff-tree":
	default:
		return nil, errors.New("command is not allowed in a temporary diff store")
	}
	if err := t.r.checkStore(t.source); err != nil {
		return nil, err
	}
	if err := t.r.checkStore(t.Path); err != nil {
		return nil, err
	}
	// Object writers may freshen an existing alternate object's mtime. Expose
	// the alternate only to the read-only renderer, never to hash/tree writes.
	var env []string
	if args[0] == "diff-tree" {
		env = []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + alternate(filepath.Join(t.source, "objects"))}
	} else if args[0] == "mktree" {
		// Historical blob IDs were validated by the snapshot reader. They need
		// not be copied into the temporary store merely to construct its tree.
		args = append(append([]string{}, args...), "--missing")
	}
	return t.r.run(ctx, t.Path, true, env, []string{"--git-dir=" + t.Path}, input, args...)
}
