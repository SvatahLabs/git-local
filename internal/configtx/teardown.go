package configtx

import (
	"bytes"
	"context"
	"errors"
	"os"

	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
)

// RemoveExcludeBlock deletes git-local's managed block from info/exclude and
// keeps the user's own lines, under the exclusive lock. It reports false when
// there is no block (already removed). A block edited by hand is refused, as
// elsewhere, so no user line is lost.
func RemoveExcludeBlock(ctx context.Context, r *gitexec.Runner, cwd string) (bool, error) {
	return excludeBlock(ctx, r, cwd, true)
}

// CheckExcludeBlock makes RemoveExcludeBlock's checks without changing
// anything, so deinit can refuse before its first change.
func CheckExcludeBlock(ctx context.Context, r *gitexec.Runner, cwd string) error {
	_, err := excludeBlock(ctx, r, cwd, false)
	return err
}

func excludeBlock(ctx context.Context, r *gitexec.Runner, cwd string, write bool) (bool, error) {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return false, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		return false, s.pendingError(pending[0])
	}
	current, err := readImage(s.common, "info/exclude")
	if err != nil || !current.Exists || !bytes.Contains(current.Data, []byte(exclude.Begin)) {
		return false, err
	}
	all, err := states(s.sources, "", nil)
	if err != nil {
		return false, err
	}
	patterns, alternates, err := s.compile(all)
	if err != nil {
		return false, err
	}
	user, err := exclude.UserBytes(current.Data, patterns, alternates...)
	if err != nil {
		return false, errors.Join(err, errors.New("restore the lines between the git-local markers in .git/info/exclude, or delete the block by hand, then run git local deinit again"))
	}
	if !write {
		return true, nil
	}
	return true, s.common.AtomicWrite("info/exclude", user, os.FileMode(current.Mode))
}
