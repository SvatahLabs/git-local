// Package restore plans and executes explicit-path restores with anchored recovery
// snapshots, durable journals and guarded filesystem writes.
package restore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
)

type Fingerprint struct {
	Exists      bool   `json:"exists"`
	Kind        string `json:"kind,omitempty"`
	GitMode     string `json:"gitMode,omitempty"`
	BlobID      string `json:"blobId,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	KeyedDigest string `json:"keyedDigest,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Permissions uint32 `json:"permissions,omitempty"`
}
type Operation struct {
	Path         string      `json:"path"`
	Action       string      `json:"action"`
	Before       Fingerprint `json:"before"`
	Encrypted    bool        `json:"encrypted,omitempty"`
	SourceBlob   string      `json:"sourceBlob,omitempty"`
	SourceDigest string      `json:"sourceDigest,omitempty"`
	SourceMode   string      `json:"sourceMode,omitempty"`
	Permissions  uint32      `json:"permissions,omitempty"`
	Dirty        bool        `json:"dirty"`
}
type Parent struct {
	Path        string `json:"path"`
	Exists      bool   `json:"exists"`
	Permissions uint32 `json:"permissions,omitempty"`
}
type planData struct {
	SchemaVersion           int                `json:"schemaVersion"`
	RepositoryID            string             `json:"repositoryId"`
	DestinationWorkspaceID  string             `json:"destinationWorkspaceId"`
	SourceWorkspaceID       string             `json:"sourceWorkspaceId"`
	SourceSnapshot          string             `json:"sourceSnapshot"`
	SourceHead              string             `json:"sourceHead"`
	DestinationBaseline     string             `json:"destinationBaseline,omitempty"`
	EncryptedKeyFingerprint string             `json:"encryptedKeyFingerprint,omitempty"`
	Selection               selection.State    `json:"selection"`
	Requests                []string           `json:"requests"`
	Ownership               selection.Evidence `json:"ownership"`
	Overwrite               bool               `json:"overwrite"`
	Parents                 []Parent           `json:"parents"`
	CreateDirectories       []string           `json:"createDirectories"`
	Operations              []Operation        `json:"operations"`
}

// RestorePlan has no public mutable backing state and no deserialization/write API.
// Accessors return copies. Execution independently builds and validates a fresh plan.
type RestorePlan struct{ data planData }

func (p RestorePlan) Operations() []Operation { return append([]Operation{}, p.data.Operations...) }
func (p RestorePlan) MarshalJSON() ([]byte, error) {
	if p.data.SchemaVersion != 1 {
		return nil, errors.New("uninitialized restore plan")
	}
	return json.Marshal(p.data)
}
func (p RestorePlan) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(b) > 16<<20 {
		return nil, errors.New("restore plan exceeds serialization limit")
	}
	return append(b, '\n'), nil
}
func (p RestorePlan) Human() string {
	var out strings.Builder
	for _, dir := range p.data.CreateDirectories {
		fmt.Fprintf(&out, "Create directory %q (0700)\n", dir)
	}
	for _, op := range p.data.Operations {
		fmt.Fprintf(&out, "%s %q", strings.ToUpper(op.Action[:1])+op.Action[1:], op.Path)
		if op.SourceBlob != "" {
			fmt.Fprintf(&out, " mode=%s blob=%s", op.SourceMode, op.SourceBlob)
		}
		if op.Dirty && op.Before.Exists {
			out.WriteString(" (overwrite local changes)")
		}
		out.WriteByte('\n')
	}
	if len(p.data.Operations) == 0 {
		out.WriteString("No changes planned.\n")
	} else {
		out.WriteString("Dry run only; no files changed.\n")
	}
	return out.String()
}
func within(parent, path string) bool { return parent == path || strings.HasPrefix(path, parent+"/") }
func scoped(path string, requests []string) bool {
	for _, p := range requests {
		if within(p, path) {
			return true
		}
	}
	return false
}
func normalizeRequests(paths []string) []string {
	sort.Strings(paths)
	result := []string{}
	for _, p := range paths {
		covered := false
		for _, old := range result {
			if within(old, p) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, p)
		}
	}
	return result
}
func validateAliases(paths []string) error {
	seen := map[string]string{}
	for _, path := range paths {
		if err := storage.ValidateSelectionPath(path); err != nil {
			return err
		}
		parts := strings.Split(path, "/")
		for i := range parts {
			p := strings.Join(parts[:i+1], "/")
			key := storage.AliasKey(p)
			if old, ok := seen[key]; ok && old != p {
				return fmt.Errorf("%w: ambiguous restore paths %q and %q", storage.ErrConflict, old, p)
			}
			seen[key] = p
		}
	}
	return nil
}
func equalFingerprint(a, b Fingerprint) bool { return a == b }
func (p RestorePlan) Equal(other RestorePlan) bool {
	a, e := p.JSON()
	b, f := other.JSON()
	return e == nil && f == nil && bytes.Equal(a, b)
}
