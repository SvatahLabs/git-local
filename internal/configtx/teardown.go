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
// keeps the user's own lines, under the exclusive lock. With keep, the
// block's patterns stay as ordinary lines (exclude.Kept), so the selected
// files stay ignored; without it, the lines an earlier deinit kept go too
// (exclude.WithoutKept). It reports whether it removed the block (false when
// there is none: already removed) and whether it removed kept lines. A block
// edited by hand is refused, as elsewhere, so no user line is lost.
func RemoveExcludeBlock(ctx context.Context, r *gitexec.Runner, cwd string, keep bool) (bool, bool, error) {
	return excludeBlock(ctx, r, cwd, true, keep)
}

// CheckExcludeBlock makes RemoveExcludeBlock's checks without changing
// anything, so deinit can refuse before its first change.
func CheckExcludeBlock(ctx context.Context, r *gitexec.Runner, cwd string) error {
	_, _, err := excludeBlock(ctx, r, cwd, false, false)
	return err
}

func excludeBlock(ctx context.Context, r *gitexec.Runner, cwd string, write, keep bool) (bool, bool, error) {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return false, false, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return false, false, err
	}
	if len(pending) > 0 {
		return false, false, s.pendingError(pending[0])
	}
	current, err := readImage(s.common, "info/exclude")
	if err != nil || !current.Exists {
		return false, false, err
	}
	if !bytes.Contains(current.Data, []byte(exclude.Begin)) {
		// A keep-mode deinit interrupted after this write, run again with
		// unignore, still removes the lines it kept.
		user, removed := exclude.WithoutKept(current.Data)
		if !write || keep || !removed {
			return false, false, nil
		}
		return false, true, s.common.AtomicWrite("info/exclude", user, os.FileMode(current.Mode))
	}
	all, err := states(s.sources, "", nil)
	if err != nil {
		return false, false, err
	}
	patterns, alternates, err := s.compile(all)
	if err != nil {
		return false, false, err
	}
	user, err := exclude.UserBytes(current.Data, patterns, alternates...)
	if err != nil {
		return false, false, errors.Join(err, errors.New("restore the lines between the git-local markers in .git/info/exclude, or delete the block by hand, then run git local deinit again"))
	}
	if !write {
		return true, false, nil
	}
	removed := false
	if keep {
		user = exclude.Kept(current.Data)
	} else {
		user, removed = exclude.WithoutKept(user)
	}
	return true, removed, s.common.AtomicWrite("info/exclude", user, os.FileMode(current.Mode))
}
