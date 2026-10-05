// Package overlay separates a named local patch from ordinary edits to a
// tracked file. Git's clean/smudge integration is layered on this merge core.
package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxText = 1 << 20

var ErrConflict = errors.New("overlay conflicts with mainline or ordinary edits")

// Strip removes the change Base -> Local from Working while retaining edits
// made after the overlay was applied. Overlapping edits are rejected.
func Strip(ctx context.Context, git string, base, local, working []byte) ([]byte, error) {
	return merge(ctx, git, base, local, working)
}

// Apply replays the change Base -> Local onto a new mainline version.
// An overlapping upstream change is a conflict, never an automatic choice.
func Apply(ctx context.Context, git string, base, local, newBase []byte) ([]byte, error) {
	return merge(ctx, git, newBase, base, local)
}

func validateText(data []byte) error {
	if len(data) > MaxText || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return errors.New("overlay requires UTF-8 text without NUL, at most 1 MiB")
	}
	return nil
}

func merge(ctx context.Context, git string, current, base, other []byte) ([]byte, error) {
	for _, data := range [][]byte{current, base, other} {
		if err := validateText(data); err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(git) {
		return nil, errors.New("Git executable must have an absolute path")
	}
	dir, err := os.MkdirTemp("", "gitlocal-overlay-merge-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	paths := []string{filepath.Join(dir, "current"), filepath.Join(dir, "base"), filepath.Join(dir, "other")}
	for i, data := range [][]byte{current, base, other} {
		if err := os.WriteFile(paths[i], data, 0600); err != nil {
			return nil, err
		}
	}
	cmd := isolatedGit(ctx, git, dir, "-c", "merge.conflictStyle=merge", "merge-file", "-p", "-L", "mainline", "-L", "overlay-base", "-L", "local-edits", paths[0], paths[1], paths[2])
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() <= 127 {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("overlay merge failed: %w", err)
	}
	if err := validateText(out); err != nil {
		return nil, err
	}
	return out, nil
}

// isolatedGit runs git on scratch files without repository, user or system
// configuration.
func isolatedGit(ctx context.Context, git, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") && key != "LC_ALL" && key != "LANG" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// hunkVersions splits the change base -> local into its hunks (without
// context) and returns, for each, base with only that hunk applied.
func hunkVersions(ctx context.Context, git string, base, local []byte) ([][]byte, error) {
	if !filepath.IsAbs(git) {
		return nil, errors.New("Git executable must have an absolute path")
	}
	dir, err := os.MkdirTemp("", "gitlocal-overlay-hunks-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for name, data := range map[string][]byte{"base": base, "local": local} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return nil, err
		}
	}
	out, err := isolatedGit(ctx, git, dir, "diff", "--no-index", "--no-color", "--no-ext-diff", "--no-textconv", "--unified=0", "--", "base", "local").Output()
	if err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("overlay diff failed: %w", err)
		}
	}
	lines := bytes.SplitAfter(base, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	type hunk struct {
		start, end int
		added      [][]byte
	}
	var hunks []hunk
	for _, line := range bytes.SplitAfter(out, []byte("\n")) {
		text := bytes.TrimSuffix(line, []byte("\n"))
		switch {
		case hunkHeader.Match(text):
			m := hunkHeader.FindSubmatch(text)
			start, _ := strconv.Atoi(string(m[1]))
			count := 1
			if len(m[2]) > 0 {
				count, _ = strconv.Atoi(string(m[2]))
			}
			h := hunk{start: start - 1, end: start - 1 + count}
			if count == 0 {
				// A pure insertion goes after line start.
				h.start, h.end = start, start
			}
			if h.start < 0 || h.end > len(lines) || h.start > h.end {
				return nil, errors.New("unexpected overlay diff hunk")
			}
			hunks = append(hunks, h)
		case len(hunks) == 0:
		case bytes.HasPrefix(text, []byte("+")):
			last := &hunks[len(hunks)-1]
			last.added = append(last.added, append(bytes.Clone(text[1:]), '\n'))
		case bytes.HasPrefix(text, []byte("\\")):
			// "No newline at end of file" after an added line.
			last := &hunks[len(hunks)-1]
			if n := len(last.added); n > 0 && bytes.HasSuffix(line, []byte("\n")) {
				last.added[n-1] = bytes.TrimSuffix(last.added[n-1], []byte("\n"))
			}
		}
	}
	versions := make([][]byte, 0, len(hunks))
	for _, h := range hunks {
		var v []byte
		for _, l := range lines[:h.start] {
			v = append(v, l...)
		}
		for _, l := range h.added {
			v = append(v, l...)
		}
		for _, l := range lines[h.end:] {
			v = append(v, l...)
		}
		versions = append(versions, v)
	}
	return versions, nil
}
