package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

const maxBrowseOutput = 16 << 20
const maxGrepBlob = 1 << 20
const maxGrepBytes = 64 << 20

type FileStat struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
}

type StatSummary struct {
	Files     int        `json:"files"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Binary    int        `json:"binary"`
	Entries   []FileStat `json:"entries"`
}

func snapshotStat(ctx context.Context, r *gitexec.Runner, store, id string) (string, StatSummary, error) {
	out, err := r.HistoryListing(ctx, store, nil, "diff-tree", "--root", "--no-commit-id", "-r", "--numstat", "-z", "--no-ext-diff", "--no-textconv", "--no-renames", id, "--", "files")
	if err != nil {
		return "", StatSummary{}, err
	}
	return parseSnapshotStat(out, "files/")
}

func parseSnapshotStat(out []byte, prefix string) (string, StatSummary, error) {
	summary := StatSummary{Entries: []FileStat{}}
	var lines strings.Builder
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := strings.SplitN(string(record), "\t", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], prefix) {
			return "", summary, errors.New("malformed historical stat")
		}
		path := strings.TrimPrefix(parts[2], prefix)
		if err := storage.ValidateSelectionPath(path); err != nil {
			return "", summary, err
		}
		summary.Files++
		if parts[0] == "-" && parts[1] == "-" {
			summary.Binary++
			summary.Entries = append(summary.Entries, FileStat{Path: path, Binary: true})
			fmt.Fprintf(&lines, "  %q binary\n", path)
			continue
		}
		add, e1 := strconv.Atoi(parts[0])
		del, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil || add < 0 || del < 0 {
			return "", summary, errors.New("invalid historical line count")
		}
		summary.Additions += add
		summary.Deletions += del
		summary.Entries = append(summary.Entries, FileStat{Path: path, Additions: add, Deletions: del})
		fmt.Fprintf(&lines, "  %q +%d -%d\n", path, add, del)
	}
	label := "files"
	if summary.Files == 1 {
		label = "file"
	}
	return fmt.Sprintf("  %d %s changed, +%d -%d, %d binary\n", summary.Files, label, summary.Additions, summary.Deletions, summary.Binary) + lines.String(), summary, nil
}

// Show reads exactly one file from a reachable user snapshot. The path is
// repository-relative and is never interpreted as a Git revision expression.
func Show(ctx context.Context, r *gitexec.Runner, cwd, source, selector, path string) ([]byte, error) {
	if err := ValidateSelector(selector); err != nil {
		return nil, err
	}
	if err := storage.ValidateSelectionPath(path); err != nil {
		return nil, err
	}
	var data []byte
	err := configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		snapshot, err := ResolveLocked(ctx, r, w, source, selector)
		if err != nil {
			return err
		}
		_, ok := snapshot.Files[path]
		if !ok {
			return fmt.Errorf("path %q is not a recorded file in snapshot %s", path, snapshot.ID)
		}
		data, err = readSnapshotFileLocked(ctx, r, w, snapshot, path, maxBrowseOutput)
		if err != nil {
			return err
		}
		return recheck()
	})
	return data, err
}

func readSnapshotFileLocked(ctx context.Context, r *gitexec.Runner, w workspace.Context, snapshot Snapshot, path string, limit int64) ([]byte, error) {
	file, ok := snapshot.Files[path]
	if !ok {
		return nil, fmt.Errorf("path %q is not a recorded file in snapshot %s", path, snapshot.ID)
	}
	secure, err := loadSecure(w)
	if err != nil {
		return nil, err
	}
	encrypted := snapshot.Metadata.EncryptedDigests[path] != ""
	if secure.covers(path) != encrypted {
		return nil, fmt.Errorf("%w: sensitive-file policy and snapshot content disagree for %q", storage.ErrConflict, path)
	}
	if !encrypted {
		return readBlob(ctx, r, w.Store, file.ID, limit)
	}
	if secure == nil || secure.policy.KeyFingerprint != snapshot.Metadata.EncryptedKeyFingerprint || !secure.covers(path) {
		return nil, fmt.Errorf("%w: missing matching sensitive-file policy for %q", storage.ErrConflict, path)
	}
	source := w
	source.WorkspaceID = snapshot.WorkspaceID
	return readEncryptedBytes(ctx, r, w.Store, file.ID, secure.key, secure.identity(source, path, file.Mode), limit)
}

func readBlob(ctx context.Context, r *gitexec.Runner, store, id string, limit int64) ([]byte, error) {
	if !validOID(id) {
		return nil, errors.New("invalid historical blob ID")
	}
	sizeData, err := r.History(ctx, store, nil, "cat-file", "-s", id)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeData)), 10, 64)
	if err != nil || size < 0 {
		return nil, errors.New("invalid historical blob size")
	}
	if size > limit {
		return nil, fmt.Errorf("historical blob exceeds %d-byte read limit", limit)
	}
	data, err := r.History(ctx, store, nil, "cat-file", "blob", id)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, errors.New("historical blob size changed during read")
	}
	return data, nil
}

// DiffSnapshots compares recorded files, never the current worktree.
func DiffSnapshots(ctx context.Context, r *gitexec.Runner, cwd, source, before, after string) (string, error) {
	if err := ValidateSelector(before); err != nil {
		return "", err
	}
	if err := ValidateSelector(after); err != nil {
		return "", err
	}
	var patch string
	err := configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		a, err := ResolveLocked(ctx, r, w, source, before)
		if err != nil {
			return err
		}
		b, err := ResolveLocked(ctx, r, w, source, after)
		if err != nil {
			return err
		}
		beforeTree, afterTree := a.FilesTree, b.FilesTree
		var diffGit historyGit = r
		diffStore := w.Store
		var temporary *gitexec.TemporaryHistory
		if len(a.Metadata.EncryptedDigests) != 0 || len(b.Metadata.EncryptedDigests) != 0 {
			temporary, err = r.TemporaryHistory(ctx, w.Store)
			if err != nil {
				return err
			}
			defer temporary.Close()
			beforeTree, err = plainSnapshotTree(ctx, r, w, a, temporary)
			if err != nil {
				return err
			}
			afterTree, err = plainSnapshotTree(ctx, r, w, b, temporary)
			if err != nil {
				return err
			}
			diffGit, diffStore = temporary, temporary.Path
		}
		out, err := diffGit.History(ctx, diffStore, nil, "diff-tree", "--no-commit-id", "-r", "-p", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--full-index", "--src-prefix=a/", "--dst-prefix=b/", beforeTree, afterTree, "--")
		if err != nil {
			return err
		}
		if !utf8.Valid(out) {
			return errors.New("historical patch contains non-UTF-8 bytes; use show for raw content")
		}
		patch = terminalText(string(out))
		return recheck()
	})
	return patch, err
}

type GrepMatch struct {
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Text     string `json:"text"`
}

type GrepResult struct {
	Matches       []GrepMatch `json:"matches"`
	SkippedBinary int         `json:"skippedBinary"`
	SkippedLarge  int         `json:"skippedLarge"`
	Truncated     bool        `json:"truncated"`
	// BudgetExhausted reports that grep stopped at its 64 MiB content budget;
	// Truncated is set too.
	BudgetExhausted bool `json:"budgetExhausted"`
	MoreSnapshots   bool `json:"moreSnapshots"`
	Searched        int  `json:"searched"`
}

// Grep searches a bounded number of reachable snapshots for a literal UTF-8
// string. Binary and oversized blobs are counted and reported, never searched.
func Grep(ctx context.Context, r *gitexec.Runner, cwd, source, pattern string, snapshots, maxMatches int) (GrepResult, error) {
	result := GrepResult{Matches: []GrepMatch{}}
	if pattern == "" || !utf8.ValidString(pattern) || strings.ContainsRune(pattern, 0) {
		return result, errors.New("grep requires a nonempty UTF-8 pattern without NUL")
	}
	if snapshots < 1 || snapshots > 100 || maxMatches < 1 || maxMatches > 1000 {
		return result, errors.New("grep limits must be 1–100 snapshots and 1–1000 matches")
	}
	err := configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		id, err := sourceWorkspace(w, source)
		if err != nil {
			return err
		}
		tip, err := head(ctx, r, w.Store, Ref(id))
		if err != nil {
			return err
		}
		finish := func() error {
			if err := recheck(); err != nil {
				return err
			}
			current, err := head(ctx, r, w.Store, Ref(id))
			if err != nil {
				return err
			}
			if current != tip {
				return fmt.Errorf("%w: snapshot history changed during grep", storage.ErrConflict)
			}
			return nil
		}
		if tip == "" {
			return finish()
		}
		list, err := r.History(ctx, w.Store, nil, "rev-list", "--first-parent", "--parents", fmt.Sprintf("--max-count=%d", snapshots+1), tip)
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(list)), "\n")
		if len(lines) > snapshots {
			result.MoreSnapshots = true
			lines = lines[:snapshots]
		}
		var scanned int64
		outputBytes := 0
		for _, line := range lines {
			if line == "" {
				continue
			}
			result.Searched++
			fields := strings.Fields(line)
			if len(fields) < 1 || len(fields) > 2 || !validOID(fields[0]) {
				return errors.New("nonlinear or malformed workspace history")
			}
			for _, parent := range fields[1:] {
				if !validOID(parent) {
					return errors.New("invalid historical parent ID")
				}
			}
			tree, metadata, err := readPrevious(ctx, r, w.Store, fields[0], w.RepositoryID, id)
			if err != nil {
				return err
			}
			snapshot, err := validateSnapshotLocked(ctx, r, w, Snapshot{ID: fields[0], WorkspaceID: id, FilesTree: tree, Metadata: metadata})
			if err != nil {
				return err
			}
			paths := make([]string, 0, len(snapshot.Files))
			for path := range snapshot.Files {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				if snapshot.Metadata.EncryptedDigests[path] == "" {
					blob := snapshot.Files[path].ID
					sizeData, err := r.History(ctx, w.Store, nil, "cat-file", "-s", blob)
					if err != nil {
						return err
					}
					size, err := strconv.ParseInt(strings.TrimSpace(string(sizeData)), 10, 64)
					if err != nil || size < 0 {
						return errors.New("invalid historical blob size")
					}
					if size > maxGrepBlob {
						result.SkippedLarge++
						continue
					}
					if scanned+size > maxGrepBytes {
						// The budget is spent; the rest goes unsearched.
						result.Truncated, result.BudgetExhausted = true, true
						return finish()
					}
				}
				limit := int64(maxGrepBlob)
				if remaining := int64(maxGrepBytes) - scanned; remaining < limit {
					limit = remaining
				}
				data, err := readSnapshotFileLocked(ctx, r, w, snapshot, path, limit)
				if err != nil {
					if errors.Is(err, errReadLimit) && limit < maxGrepBlob {
						result.Truncated, result.BudgetExhausted = true, true
						return finish()
					}
					if errors.Is(err, errReadLimit) {
						result.SkippedLarge++
						continue
					}
					return err
				}
				scanned += int64(len(data))
				if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
					result.SkippedBinary++
					continue
				}
				for i, row := range strings.Split(string(data), "\n") {
					if strings.Contains(row, pattern) {
						if len(result.Matches) == maxMatches {
							result.Truncated = true
							return finish()
						}
						outputBytes += len(snapshot.ID) + len(strconv.Quote(path)) + len(strconv.Itoa(i+1)) + len(terminalText(row)) + 4
						if outputBytes > maxBrowseOutput {
							result.Truncated = true
							return finish()
						}
						result.Matches = append(result.Matches, GrepMatch{Snapshot: snapshot.ID, Path: path, Line: i + 1, Text: row})
					}
				}
			}
		}
		return finish()
	})
	return result, err
}

func (r GrepResult) Human() string {
	var out strings.Builder
	for _, match := range r.Matches {
		fmt.Fprintf(&out, "%s:%q:%d:%s\n", match.Snapshot, match.Path, match.Line, terminalText(match.Text))
	}
	if len(r.Matches) == 0 {
		out.WriteString("No matches.\n")
	}
	if r.SkippedBinary > 0 || r.SkippedLarge > 0 {
		fmt.Fprintf(&out, "Skipped %d binary and %d oversized historical files.\n", r.SkippedBinary, r.SkippedLarge)
	}
	if r.BudgetExhausted {
		out.WriteString("Searched the 64 MiB content limit; later files and snapshots were not searched.\n")
	} else if r.Truncated {
		out.WriteString("Match limit reached; results truncated.\n")
	}
	if r.MoreSnapshots {
		fmt.Fprintf(&out, "Searched newest %d snapshots; older history was not searched.\n", r.Searched)
	}
	return out.String()
}
