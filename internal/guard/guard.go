package guard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/overlay"
	"gitlocal/internal/storage"
)

type Finding struct {
	configtx.HookConflict
	Commit, OverlayName, OverlayReason string
}

func names(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("Git path list is not NUL terminated")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		paths = append(paths, string(part))
	}
	return paths, nil
}

func Staged(ctx context.Context, r *gitexec.Runner, cwd, index string) ([]Finding, error) {
	snapshot, err := configtx.LoadGuardSnapshot(ctx, r, cwd)
	if errors.Is(err, configtx.ErrNoState) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := r.Guard(ctx, snapshot.Root, index, "diff", "--cached", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--diff-filter=ACMRTUXB")
	if err != nil {
		return nil, err
	}
	paths, err := names(b)
	if err != nil {
		return nil, err
	}
	conflicts, err := snapshot.Check(paths)
	if err != nil {
		return nil, err
	}
	findings := make([]Finding, 0, len(conflicts))
	for _, c := range conflicts {
		findings = append(findings, Finding{HookConflict: c})
	}
	versions, err := overlay.LoadForPaths(ctx, snapshot.Common, "", paths)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return findings, nil
	}
	processor, err := overlay.NewProcessor(ctx, r, snapshot.Root, index)
	if err != nil {
		return nil, err
	}
	adopted, err := adoptedRevisions(ctx, r, snapshot.Root)
	if err != nil {
		return nil, err
	}
	for _, version := range versions {
		if version.Manifest.WorkspaceID != processor.WorkspaceID {
			continue
		}
		for _, staged := range aliasPaths(paths, version.Manifest.Path) {
			// Read the blob directly: the path comes from Git, and an alias
			// spelling such as conf\app.conf fails IndexBlob's checks.
			candidate, err := r.Guard(ctx, snapshot.Root, index, "cat-file", "blob", ":0:"+staged)
			if err != nil {
				return nil, err
			}
			previous, err := committedBlobs(ctx, r, snapshot.Root, staged, adopted...)
			if err != nil {
				return nil, err
			}
			if reason, err := overlayReason(ctx, r, version, candidate, previous); err != nil {
				return nil, err
			} else if reason != "" {
				findings = append(findings, Finding{HookConflict: configtx.HookConflict{Path: staged, WorkspaceID: version.Manifest.WorkspaceID}, OverlayName: version.Manifest.Name, OverlayReason: reason})
			}
		}
	}
	return findings, nil
}

// adoptedRevisions lists the revisions whose content a commit may carry
// without leaking an overlay: a merge's other side, HEAD's parents, and HEAD
// itself once a remote has it. An unpublished HEAD alone doesn't count: after
// a git commit --no-verify, it holds the leaked hunk itself.
func adoptedRevisions(ctx context.Context, r *gitexec.Runner, root string) ([]string, error) {
	revisions := []string{"MERGE_HEAD"}
	_, err := r.Guard(ctx, root, "", "rev-parse", "--verify", "--quiet", "HEAD")
	var ge *gitexec.Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return revisions, nil // An unborn branch.
	}
	if err != nil {
		return nil, err
	}
	parents, err := r.Guard(ctx, root, "", "rev-parse", "HEAD^@")
	if err != nil {
		return nil, err
	}
	revisions = append(revisions, strings.Fields(string(parents))...)
	published, err := r.Guard(ctx, root, "", "for-each-ref", "--count=1", "--contains", "HEAD", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(published)) != 0 {
		revisions = append(revisions, "HEAD")
	}
	return revisions, nil
}

// aliasPaths returns the changed paths that matched an overlay's path by alias
// key: its exact spelling and any case, normalization or portable alias of it.
// Each is checked, so a patch staged under an alias spelling is still caught.
func aliasPaths(paths []string, overlayPath string) []string {
	want := storage.AliasKey(overlayPath)
	var matched []string
	for _, p := range paths {
		candidate := p
		if storage.ValidateSelectionPath(p) != nil {
			candidate = storage.PortableAlias(p)
		}
		if storage.AliasKey(candidate) == want {
			matched = append(matched, p)
		}
	}
	return matched
}

// committedBlobs returns path's content in each revision that has it as a file.
func committedBlobs(ctx context.Context, r *gitexec.Runner, root, path string, revisions ...string) ([][]byte, error) {
	var result [][]byte
	for _, rev := range revisions {
		id, err := r.Guard(ctx, root, "", "rev-parse", "--verify", "--quiet", rev+":"+path)
		var ge *gitexec.Error
		if errors.As(err, &ge) && ge.Code == 1 {
			continue // An unborn HEAD or a path the revision lacks.
		}
		if err != nil {
			return nil, err
		}
		object := strings.TrimSpace(string(id))
		kind, err := r.Guard(ctx, root, "", "cat-file", "-t", object)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(kind)) != "blob" {
			continue
		}
		data, err := r.Guard(ctx, root, "", "cat-file", "blob", object)
		if err != nil {
			return nil, err
		}
		result = append(result, data)
	}
	return result, nil
}

func overlayReason(ctx context.Context, r *gitexec.Runner, version overlay.Version, candidate []byte, previous [][]byte) (string, error) {
	contains, err := overlay.ContainsPatch(ctx, r.Executable(), version, candidate, previous...)
	if errors.Is(err, overlay.ErrConflict) {
		return "content overlaps the saved overlay and cannot be verified", nil
	}
	if err != nil {
		return "", err
	}
	if contains {
		return "content already contains the saved overlay", nil
	}
	return "", nil
}

var objectID = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

func Push(ctx context.Context, r *gitexec.Runner, cwd, remote string, input io.Reader) ([]Finding, error) {
	if remote == "" {
		return nil, errors.New("remote name is required")
	}
	return push(ctx, r, cwd, remote, input, false)
}

// push checks the commits that pre-push ref updates would publish. With full,
// it checks each update's whole ancestry instead of excluding what the remote
// is known to have.
func push(ctx context.Context, r *gitexec.Runner, cwd, remote string, input io.Reader, full bool) ([]Finding, error) {
	snapshot, err := configtx.LoadGuardSnapshot(ctx, r, cwd)
	if errors.Is(err, configtx.ErrNoState) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(input, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("pre-push input exceeds safety limit")
	}
	seen := map[string]bool{}
	findings := []Finding{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 || !objectID.MatchString(fields[1]) || !objectID.MatchString(fields[3]) {
			return nil, fmt.Errorf("invalid pre-push ref update %q", line)
		}
		local, upstream := fields[1], fields[3]
		if allZero(local) {
			continue
		} // Deleting a remote ref cannot introduce a leak.
		args := []string{"rev-list", local}
		if !full {
			known := !allZero(upstream)
			if known {
				// The remote's old commit is missing when someone else pushed
				// and this repository hasn't fetched since, as in a force push.
				if known, err = hasCommit(ctx, r, snapshot.Root, upstream); err != nil {
					return nil, err
				}
			}
			if known {
				args = append(args, "^"+upstream)
			} else if !strings.ContainsAny(remote, "/\\") && remote != "." && remote != ".." {
				// A new remote branch, or one whose old commit is unknown, may
				// share ancestry with branches already on this remote. Exclude
				// those known remote-tracking refs so an old, already-published
				// leak does not block an unrelated new branch.
				args = append(args, "--not", "--remotes="+remote)
			}
		}
		b, err := r.Guard(ctx, snapshot.Root, "", args...)
		if err != nil {
			return nil, err
		}
		for _, commit := range strings.Fields(string(b)) {
			if seen[commit] {
				continue
			}
			seen[commit] = true
			pathsRaw, err := r.Guard(ctx, snapshot.Root, "", "diff-tree", "--root", "-m", "--no-commit-id", "--name-only", "-r", "-z", "--no-renames", "--diff-filter=ACMRTUXB", commit)
			if err != nil {
				return nil, err
			}
			paths, err := names(pathsRaw)
			if err != nil {
				return nil, err
			}
			conflicts, err := snapshot.Check(paths)
			if err != nil {
				return nil, err
			}
			for _, c := range conflicts {
				findings = append(findings, Finding{HookConflict: c, Commit: commit})
			}
			versions, err := overlay.LoadForPaths(ctx, snapshot.Common, "", paths)
			if err != nil {
				return nil, err
			}
			var parents []string
			if len(versions) > 0 {
				b, err := r.Guard(ctx, snapshot.Root, "", "rev-parse", commit+"^@")
				if err != nil {
					return nil, err
				}
				parents = strings.Fields(string(b))
			}
			for _, version := range versions {
				for _, pushed := range aliasPaths(paths, version.Manifest.Path) {
					candidate, err := r.Guard(ctx, snapshot.Root, "", "cat-file", "blob", commit+":"+pushed)
					if err != nil {
						return nil, err
					}
					previous, err := committedBlobs(ctx, r, snapshot.Root, pushed, parents...)
					if err != nil {
						return nil, err
					}
					reason, err := overlayReason(ctx, r, version, candidate, previous)
					if err != nil {
						return nil, err
					}
					if reason != "" {
						findings = append(findings, Finding{HookConflict: configtx.HookConflict{Path: pushed, WorkspaceID: version.Manifest.WorkspaceID}, Commit: commit, OverlayName: version.Manifest.Name, OverlayReason: reason})
					}
				}
			}
		}
	}
	return findings, nil
}

func allZero(id string) bool { return strings.Trim(id, "0") == "" }

// hasCommit reports whether the repository has the commit id.
func hasCommit(ctx context.Context, r *gitexec.Runner, root, id string) (bool, error) {
	_, err := r.Guard(ctx, root, "", "cat-file", "-e", id+"^{commit}")
	var ge *gitexec.Error
	if errors.As(err, &ge) && (ge.Code == 1 || ge.Code == 128) {
		return false, nil
	}
	return err == nil, err
}

// AuditUpstream scans local branches that declare an upstream. Doctor uses a
// full ancestry scan so an already-pushed leak is visible too.
func AuditUpstream(ctx context.Context, r *gitexec.Runner, cwd string) ([]Finding, error) {
	b, err := r.Guard(ctx, cwd, "", "for-each-ref", "--format=%(refname) %(upstream) %(objectname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := []Finding{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !objectID.MatchString(fields[2]) {
			continue
		}
		input := strings.NewReader(fields[0] + " " + fields[2] + " " + fields[1] + " " + strings.Repeat("0", len(fields[2])) + "\n")
		findings, err := push(ctx, r, cwd, "", input, true)
		if err != nil {
			return nil, err
		}
		for _, finding := range findings {
			key := finding.Commit + "\x00" + finding.Path
			if !seen[key] {
				seen[key] = true
				result = append(result, finding)
			}
		}
	}
	return result, nil
}
