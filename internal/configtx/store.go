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
		return s.pendingError(pending[0])
	}
	return fn(*s.w)
}

// ReadStore runs fn under the shared lock, for reading the store while
// guards and the overlay filter, which take the shared lock too, can run. It
// doesn't refuse a pending transaction: maintenance checks the store it just
// packed, and a transaction another command started since (or left
// interrupted) doesn't change whether that store is sound.
func ReadStore(ctx context.Context, r *gitexec.Runner, cwd string, fn func(workspace.Context) error) error {
	s, err := openMode(ctx, r, cwd, true)
	if err != nil {
		return err
	}
	defer s.close()
	return fn(*s.w)
}
