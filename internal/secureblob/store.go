package secureblob

import (
	"errors"
	"os"

	"github.com/SvatahLabs/git-local/internal/storage"
)

const PolicyFile = "gitlocal/encryption.json"

// ReadLocked loads the common policy from an anchored Git directory. The
// caller must hold the repository's common lock while using the result. A
// policy whose key is unavailable still exists: it returns true with an error
// matching ErrKeyUnavailable.
func ReadLocked(common *storage.Dir, repoRoot, commonGitDir string) (Policy, bool, error) {
	var policy Policy
	if err := common.ReadJSON(PolicyFile, &policy); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Policy{}, false, nil
		}
		return Policy{}, false, err
	}
	canonical, err := Canonical(policy, repoRoot, commonGitDir)
	if err != nil {
		return Policy{}, errors.Is(err, ErrKeyUnavailable), err
	}
	return canonical, true, nil
}

// WriteLocked publishes a policy with a durable atomic replacement. The caller
// must hold the common lock and either prove the store is empty or journal and
// complete history migration first; this function cannot make migration atomic.
func WriteLocked(common *storage.Dir, policy Policy, repoRoot, commonGitDir string) error {
	canonical, err := Canonical(policy, repoRoot, commonGitDir)
	if err != nil {
		return err
	}
	if _, err := LoadKey(canonical, repoRoot, commonGitDir); err != nil {
		return err
	}
	return common.WriteJSON(PolicyFile, canonical)
}

// Published reports whether an encryption migration's policy is in service.
// A first migration (previous is nil) publishes by creating the policy file; a
// correction replaces previous, so the file must hold one of the two. The
// file is read without resolving the key path, which may be unavailable.
func Published(common *storage.Dir, next Policy, previous *Policy) (bool, error) {
	var live Policy
	if err := common.ReadJSON(PolicyFile, &live); errors.Is(err, os.ErrNotExist) {
		if previous != nil {
			return false, errors.New("sensitive-file policy is missing during its correction")
		}
		return false, nil
	} else if err != nil {
		return false, err
	}
	switch {
	case previous == nil || SameRules(live, next):
		return true, nil
	case SameRules(live, *previous):
		return false, nil
	}
	return false, errors.New("sensitive-file policy differs from both versions in the encryption migration journal")
}
