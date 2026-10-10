package secureblob

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitlocal/internal/keymaterial"
	"gitlocal/internal/storage"
)

const PolicySchemaVersion = 1

type PathRule struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// Policy is common to all workspaces in one repository. Key bytes are never
// serialized here; KeyFile names an external owner-private file.
type Policy struct {
	SchemaVersion  int        `json:"schemaVersion"`
	KeyFile        string     `json:"keyFile"`
	KeyFingerprint string     `json:"keyFingerprint"`
	Sensitive      []PathRule `json:"sensitive"`
}

func resolvedPath(path string) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func validFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// Canonical validates and sorts a policy. It refuses a key in either the
// checkout or common Git directory, including paths through symlinked parents.
func Canonical(policy Policy, repoRoot, commonGitDir string) (Policy, error) {
	if policy.SchemaVersion != PolicySchemaVersion {
		return Policy{}, errors.New("unsupported sensitive-file policy schema")
	}
	if !filepath.IsAbs(policy.KeyFile) || !validFingerprint(policy.KeyFingerprint) {
		return Policy{}, errors.New("sensitive-file policy needs an absolute key path and key fingerprint")
	}
	if len(policy.Sensitive) == 0 {
		return Policy{}, errors.New("sensitive-file policy has no paths")
	}
	keyPath, err := resolvedPath(policy.KeyFile)
	if err != nil {
		return Policy{}, err
	}
	for _, root := range []string{repoRoot, commonGitDir} {
		if inside, err := keymaterial.Inside(root, keyPath); err != nil {
			return Policy{}, err
		} else if inside {
			return Policy{}, errors.New("sensitive-file key must be outside the checkout and common Git directory")
		}
	}
	result := policy
	result.KeyFile = keyPath
	result.Sensitive = append([]PathRule{}, policy.Sensitive...)
	sort.Slice(result.Sensitive, func(i, j int) bool { return result.Sensitive[i].Path < result.Sensitive[j].Path })
	aliases := map[string]string{}
	for i, rule := range result.Sensitive {
		if rule.Kind != "file" && rule.Kind != "directory" {
			return Policy{}, fmt.Errorf("invalid sensitive path kind for %q", rule.Path)
		}
		if err := storage.ValidateSelectionPath(rule.Path); err != nil {
			return Policy{}, err
		}
		if strings.Contains(rule.Path, "\\") {
			return Policy{}, errors.New("sensitive paths must use slash separators")
		}
		for _, prior := range result.Sensitive[:i] {
			if prior.Path == rule.Path || prior.Kind == "directory" && (rule.Path == prior.Path || strings.HasPrefix(rule.Path, prior.Path+"/")) {
				return Policy{}, fmt.Errorf("overlapping sensitive path %q", rule.Path)
			}
		}
		parts := strings.Split(rule.Path, "/")
		for j := range parts {
			prefix := strings.Join(parts[:j+1], "/")
			key := storage.AliasKey(prefix)
			if old, ok := aliases[key]; ok && old != prefix {
				return Policy{}, fmt.Errorf("ambiguous sensitive paths %q and %q", old, prefix)
			}
			aliases[key] = prefix
		}
	}
	return result, nil
}

func (policy Policy) Covers(path string) bool {
	for _, rule := range policy.Sensitive {
		if rule.Path == path || rule.Kind == "directory" && strings.HasPrefix(path, rule.Path+"/") {
			return true
		}
	}
	return false
}

// UncoveredAlias returns the sensitive rule a path falls under by alias key
// (case, normalization, or a file rule naming a directory) when the path's
// exact spelling is not covered, so it would otherwise be stored as plaintext.
func (policy Policy) UncoveredAlias(path string) (string, bool) {
	if policy.Covers(path) {
		return "", false
	}
	key := storage.AliasKey(path)
	for _, rule := range policy.Sensitive {
		ruleKey := storage.AliasKey(rule.Path)
		if key == ruleKey || strings.HasPrefix(key, ruleKey+"/") {
			return rule.Path, true
		}
	}
	return "", false
}

// LoadKey fails closed if a key is missing, changed, or not owner-private.
func LoadKey(policy Policy, repoRoot, commonGitDir string) ([]byte, error) {
	canonical, err := Canonical(policy, repoRoot, commonGitDir)
	if err != nil {
		return nil, err
	}
	key, err := keymaterial.Load(canonical.KeyFile, repoRoot)
	if err != nil {
		return nil, err
	}
	fingerprint, err := keymaterial.Fingerprint(key)
	if err != nil {
		return nil, err
	}
	if fingerprint != canonical.KeyFingerprint {
		return nil, errors.New("sensitive-file key fingerprint mismatch")
	}
	return key, nil
}

// PolicyForKey constructs a policy for a previously created external key.
func PolicyForKey(keyFile, repoRoot, commonGitDir string, rules []PathRule) (Policy, error) {
	if !filepath.IsAbs(keyFile) {
		return Policy{}, errors.New("sensitive-file key path must be absolute")
	}
	key, err := keymaterial.Load(keyFile, repoRoot)
	if err != nil {
		return Policy{}, err
	}
	fingerprint, err := keymaterial.Fingerprint(key)
	if err != nil {
		return Policy{}, err
	}
	return Canonical(Policy{SchemaVersion: PolicySchemaVersion, KeyFile: keyFile, KeyFingerprint: fingerprint, Sensitive: rules}, repoRoot, commonGitDir)
}

// RebindKeyPath permits moving the key file only when its bytes match the
// fingerprint already recorded in the policy.
func RebindKeyPath(policy Policy, keyFile, repoRoot, commonGitDir string) (Policy, error) {
	policy.KeyFile = keyFile
	if _, err := LoadKey(policy, repoRoot, commonGitDir); err != nil {
		return Policy{}, err
	}
	return Canonical(policy, repoRoot, commonGitDir)
}

// MissingKey reports a missing external file without weakening LoadKey's
// authentication checks. It is intended for diagnostic output only.
func MissingKey(policy Policy) bool {
	_, err := os.Lstat(policy.KeyFile)
	return errors.Is(err, os.ErrNotExist)
}

// SameRules reports whether two policies use the same key fingerprint and
// sensitive rules, whatever path the key file has.
func SameRules(a, b Policy) bool {
	if a.SchemaVersion != b.SchemaVersion || a.KeyFingerprint != b.KeyFingerprint || len(a.Sensitive) != len(b.Sensitive) {
		return false
	}
	sorted := func(rules []PathRule) []PathRule {
		rules = append([]PathRule{}, rules...)
		sort.Slice(rules, func(i, j int) bool { return rules[i].Path < rules[j].Path })
		return rules
	}
	left, right := sorted(a.Sensitive), sorted(b.Sensitive)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// Narrowed returns an existing rule that a corrected policy no longer covers.
// A rule is kept by a directory rule at its path or an ancestor, or, for a
// file rule, by any rule at its path; paths compare by alias key, so a rule
// may be respelled to match the checkout. Files saved under an exact spelling
// a respelled rule drops still fail the rewrite, which never decrypts history.
func Narrowed(previous, next Policy) (PathRule, bool) {
	for _, old := range previous.Sensitive {
		oldKey := storage.AliasKey(old.Path)
		kept := false
		for _, rule := range next.Sensitive {
			key := storage.AliasKey(rule.Path)
			if rule.Kind == "directory" && (key == oldKey || strings.HasPrefix(oldKey, key+"/")) || old.Kind == "file" && key == oldKey {
				kept = true
				break
			}
		}
		if !kept {
			return old, true
		}
	}
	return PathRule{}, false
}
