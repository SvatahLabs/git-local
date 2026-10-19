package configtx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/SvatahLabs/git-local/internal/exclude"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// WithRestore holds the mutation lock without traversing unrelated selected files.
// Only the explicitly requested recovery ID may remain pending. The digest binds
// all workspace registrations/selections and the common ignore file across replay.
func WithRestore(ctx context.Context, r *gitexec.Runner, cwd, id string, fn func(workspace.Context, selection.State, string, func() error) error) error {
	s, err := open(ctx, r, cwd)
	if err != nil {
		return err
	}
	defer s.close()
	pendingCheck := func() error {
		ids, e := s.pending()
		if e != nil {
			return e
		}
		for _, other := range ids {
			if other != id {
				return s.pendingError(other)
			}
		}
		return nil
	}
	if err = pendingCheck(); err != nil {
		return err
	}

	initial, err := s.stateDigest()
	if err != nil {
		return err
	}
	var state selection.State
	for _, src := range s.sources {
		if src.ID == s.w.WorkspaceID {
			state, err = src.state()
			if err != nil {
				return err
			}
		}
	}
	recheck := func() error {
		now, e := s.stateDigest()
		if e != nil {
			return e
		}
		if now != initial {
			return fmt.Errorf("%w: restore configuration changed", storage.ErrConflict)
		}
		return nil
	}
	return fn(*s.w, state, initial, recheck)
}

// TransactionKind selects the recovery handler; handlers independently re-open
// and validate their journal under the exclusive lock before making changes.
func TransactionKind(ctx context.Context, r *gitexec.Runner, cwd, id string) (string, error) {
	if !storage.ValidUUID(id) {
		return "", errors.New("invalid transaction ID")
	}
	s, err := openMode(ctx, r, cwd, true)
	if err != nil {
		return "", err
	}
	defer s.close()
	found := ""
	for _, kind := range []string{"config", "restore", "seed"} {
		for _, dir := range []string{"transactions/", "transactions/completed/"} {
			_, e := s.meta.Lstat(dir + kind + "-" + id + ".json")
			if e == nil {
				if found != "" {
					return "", errors.New("ambiguous transaction journals")
				}
				found = kind
			} else if !os.IsNotExist(e) {
				return "", e
			}
		}
	}
	if found == "" {
		return "", os.ErrNotExist
	}
	return found, nil
}

func (s *session) stateDigest() (string, error) {
	sources, e := s.readSources()
	if e != nil {
		return "", e
	}
	if !reflect.DeepEqual(sources, s.sources) {
		return "", fmt.Errorf("%w: workspace configuration changed during restore session", storage.ErrConflict)
	}
	before, e := readImage(s.common, "info/exclude")
	if e != nil {
		return "", e
	}
	all, e := states(sources, "", nil)
	if e != nil {
		return "", e
	}
	patterns, alternates, e := s.compile(all)
	if e != nil {
		return "", e
	}
	if _, e = exclude.UserBytes(before.Data, patterns, alternates...); e != nil {
		return "", e
	}
	b, e := json.Marshal(struct {
		Sources []Source
		Exclude Image
	}{sources, before})
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
