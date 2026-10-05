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
	for _, version := range versions {
		if version.Manifest.WorkspaceID != processor.WorkspaceID {
			continue
		}
		candidate, err := r.IndexBlob(ctx, snapshot.Root, index, version.Manifest.Path)
		if err != nil {
			return nil, err
		}
		if reason, err := overlayReason(ctx, r, version, candidate); err != nil {
			return nil, err
		} else if reason != "" {
			findings = append(findings, Finding{HookConflict: configtx.HookConflict{Path: version.Manifest.Path, WorkspaceID: version.Manifest.WorkspaceID}, OverlayName: version.Manifest.Name, OverlayReason: reason})
		}
	}
	return findings, nil
}

func overlayReason(ctx context.Context, r *gitexec.Runner, version overlay.Version, candidate []byte) (string, error) {
	contains, err := overlay.ContainsPatch(ctx, r.Executable(), version, candidate)
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
	snapshot, err := configtx.LoadGuardSnapshot(ctx, r, cwd)
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
		if !allZero(upstream) {
			args = append(args, "^"+upstream)
		} else if !strings.ContainsAny(remote, "/\\") && remote != "." && remote != ".." {
			// A new remote branch may share ancestry with branches already on
			// this remote. Exclude those known remote-tracking refs so an old,
			// already-published leak does not block an unrelated new branch.
			args = append(args, "--not", "--remotes="+remote)
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
			for _, version := range versions {
				candidate, err := r.Guard(ctx, snapshot.Root, "", "cat-file", "blob", commit+":"+version.Manifest.Path)
				if err != nil {
					return nil, err
				}
				reason, err := overlayReason(ctx, r, version, candidate)
				if err != nil {
					return nil, err
				}
				if reason != "" {
					findings = append(findings, Finding{HookConflict: configtx.HookConflict{Path: version.Manifest.Path, WorkspaceID: version.Manifest.WorkspaceID}, Commit: commit, OverlayName: version.Manifest.Name, OverlayReason: reason})
				}
			}
		}
	}
	return findings, nil
}

func allZero(id string) bool { return strings.Trim(id, "0") == "" }

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
		findings, err := Push(ctx, r, cwd, "upstream", input)
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
