package overlay

import (
	"bytes"
	"context"
	"strings"

	"github.com/SvatahLabs/git-local/internal/gitexec"
)

// autocrlf returns the effective core.autocrlf when it converts line
// endings, reading system and global configuration as Git does: Git for
// Windows sets it to true in its system configuration. An overlay stores the
// index blob as its base and the working file as its local version, so a
// conversion turns the whole file into the patch, and later checkouts
// conflict.
func autocrlf(ctx context.Context, r *gitexec.Runner, root string) (string, error) {
	admin, err := resolveGitPath(ctx, r, root, "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	cfg, err := r.Main(ctx, root, admin, "config", "--null", "--list")
	if err != nil {
		return "", err
	}
	value := ""
	for _, record := range bytes.Split(cfg, []byte{0}) {
		key, v, hasValue := strings.Cut(string(record), "\n")
		if strings.EqualFold(key, "core.autocrlf") {
			value = strings.ToLower(v)
			if !hasValue {
				value = "true" // A bare key is true.
			}
		}
	}
	switch value {
	case "true", "yes", "on", "1", "input":
		return value, nil
	}
	return "", nil
}
