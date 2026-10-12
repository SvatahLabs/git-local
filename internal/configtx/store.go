package configtx

import (
	"context"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/workspace"
)

// WithStore holds the exclusive repository lock, which every git-local
// command takes (shared or exclusive) before it uses the private store, and
// refuses while any transaction is pending. Store maintenance runs in it.
func WithStore(ctx context.Context, r *gitexec.Runner, cwd string, fn func(workspace.Context) error) error {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return pendingError(pending[0])
	}
	return fn(*s.w)
}
