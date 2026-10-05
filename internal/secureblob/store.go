package secureblob

import (
	"errors"
	"os"

	"gitlocal/internal/storage"
)

const PolicyFile = "gitlocal/encryption.json"

// ReadLocked loads the common policy from an anchored Git directory. The
// caller must hold the repository's common lock while using the result.
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
		return Policy{}, false, err
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
