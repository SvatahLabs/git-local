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
	cmd := exec.CommandContext(ctx, git, "-c", "merge.conflictStyle=merge", "merge-file", "-p", "-L", "mainline", "-L", "overlay-base", "-L", "local-edits", paths[0], paths[1], paths[2])
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") && key != "LC_ALL" && key != "LANG" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0")
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
