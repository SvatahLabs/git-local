package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

func validRemoteName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	for _, ch := range name {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func effectiveURLs(ctx context.Context, r *gitexec.Runner, w workspace.Context, name string, push bool) ([]string, error) {
	args := []string{"remote", "get-url"}
	if push {
		args = append(args, "--push")
	}
	args = append(args, "--all", name)
	out, err := r.Main(ctx, w.Root, w.Admin, args...)
	if err != nil {
		return nil, fmt.Errorf("read effective URL for remote %q: %w", name, err)
	}
	text := strings.TrimSuffix(string(out), "\n")
	if text == "" || strings.ContainsRune(text, 0) {
		return nil, errors.New("remote has no usable URL")
	}
	urls := strings.Split(text, "\n")
	for _, raw := range urls {
		if raw == "" || strings.ContainsRune(raw, '\r') {
			return nil, errors.New("invalid effective remote URL list")
		}
	}
	return urls, nil
}

// EffectiveDestination validates Git-expanded URLs immediately before a push.
// Both fetch and push URLs of origin are considered because either may refer to
// a host that must never receive a backup.
func EffectiveDestination(ctx context.Context, r *gitexec.Runner, w workspace.Context, name string) ([]string, error) {
	if !validRemoteName(name) || name == "origin" {
		return nil, errors.New("choose a named backup remote other than origin")
	}
	destination, err := effectiveURLs(ctx, r, w, name, true)
	if err != nil {
		return nil, err
	}
	originFetch, err := effectiveURLs(ctx, r, w, "origin", false)
	if err != nil {
		return nil, fmt.Errorf("origin is required to verify the backup host: %w", err)
	}
	originPush, err := effectiveURLs(ctx, r, w, "origin", true)
	if err != nil {
		return nil, err
	}
	if err := ValidateDestination(name, destination, append(originFetch, originPush...)); err != nil {
		return nil, err
	}
	return destination, nil
}
