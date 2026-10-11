package guard

import (
	"bufio"
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

// OverlapReason is the overlay finding that re-staging can't fix: the content
// overlaps the saved patch, so the overlay has to be saved again.
const OverlapReason = "content overlaps the saved overlay and cannot be verified"

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
// without leaking an overlay: a merge's other side, HEAD's parents and HEAD
// itself, but only those a remote-tracking ref already contains. An
// unpublished revision doesn't count, however it relates to HEAD: after a
// git commit --no-verify, it (or a descendant's parent) holds the leaked hunk
// itself.
func adoptedRevisions(ctx context.Context, r *gitexec.Runner, root string) ([]string, error) {
	var candidates []string
	if _, err := r.Guard(ctx, root, "", "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); err == nil {
		candidates = append(candidates, "MERGE_HEAD")
	} else if ge := (*gitexec.Error)(nil); !errors.As(err, &ge) || ge.Code != 1 {
		return nil, err
	}
	_, err := r.Guard(ctx, root, "", "rev-parse", "--verify", "--quiet", "HEAD")
	var ge *gitexec.Error
	if err != nil && !(errors.As(err, &ge) && ge.Code == 1) {
		return nil, err
	}
	if err == nil {
		parents, err := r.Guard(ctx, root, "", "rev-parse", "HEAD^@")
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, strings.Fields(string(parents))...)
		candidates = append(candidates, "HEAD")
	}
	var revisions []string
	for _, candidate := range candidates {
		published, err := r.Guard(ctx, root, "", "for-each-ref", "--count=1", "--contains", candidate, "--format=%(refname)", "refs/remotes")
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(published)) != 0 {
			revisions = append(revisions, candidate)
		}
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
		return OverlapReason, nil
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
	// Every ref update goes into one rev-list, so a push of many refs
	// (--mirror, --tags) costs a fixed number of Git processes. Excluding
	// everything the remote has from all updates at once is what each update
	// would exclude anyway: a commit the remote has isn't newly published.
	var locals, upstreams []string
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 4096), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 || !objectID.MatchString(fields[1]) || !objectID.MatchString(fields[3]) {
			return nil, fmt.Errorf("invalid pre-push ref update %q", line)
		}
		if allZero(fields[1]) {
			continue
		} // Deleting a remote ref cannot introduce a leak.
		locals = append(locals, fields[1])
		upstreams = append(upstreams, fields[3])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read pre-push ref updates: %w", err)
	}
	if len(locals) == 0 {
		return []Finding{}, nil
	}
	revs := strings.Join(locals, "\n") + "\n"
	if !full {
		known, err := knownCommits(ctx, r, snapshot.Root, upstreams)
		if err != nil {
			return nil, err
		}
		unknown := false
		for _, upstream := range upstreams {
			if known[upstream] {
				revs += "^" + upstream + "\n"
			} else {
				unknown = true
			}
		}
		// A new remote branch, or one whose old commit is missing because
		// someone else pushed and this repository hasn't fetched (as in a
		// force push), may share ancestry with branches already on this
		// remote. Excluding those known remote-tracking refs keeps an old,
		// already-published leak from blocking an unrelated new branch.
		if unknown && !strings.ContainsAny(remote, "/\\") && remote != "" && remote != "." && remote != ".." {
			revs += "--not\n--remotes=" + remote + "\n"
		}
	}
	listed, err := r.GuardInput(ctx, snapshot.Root, strings.NewReader(revs), "rev-list", "--stdin")
	if err != nil {
		return nil, err
	}
	commits := strings.Fields(string(listed))
	changed, err := changedPaths(ctx, r, snapshot.Root, commits)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	for _, commit := range commits {
		paths := changed[commit]
		if len(paths) == 0 {
			continue
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
	return findings, nil
}

// changedPaths runs one diff-tree over commits and returns the paths each
// adds or changes against any parent. The format puts an empty token before
// every commit ID, which no path can be, and Git ends that header line with a
// newline the first path carries.
func changedPaths(ctx context.Context, r *gitexec.Runner, root string, commits []string) (map[string][]string, error) {
	result := map[string][]string{}
	if len(commits) == 0 {
		return result, nil
	}
	out, err := r.GuardInput(ctx, root, strings.NewReader(strings.Join(commits, "\n")+"\n"), "diff-tree", "--stdin", "--root", "-m", "-r", "-z", "--name-only", "--no-renames", "--diff-filter=ACMRTUXB", "--format=%x00%H")
	if err != nil {
		return nil, err
	}
	tokens, err := names(out)
	if err != nil {
		return nil, err
	}
	current, first := "", false
	for i := 0; i < len(tokens); i++ {
		if tokens[i] == "" {
			if i+1 == len(tokens) || !objectID.MatchString(tokens[i+1]) {
				return nil, errors.New("invalid Git commit change list")
			}
			current, first = tokens[i+1], true
			i++
			continue
		}
		path := tokens[i]
		if first {
			if !strings.HasPrefix(path, "\n") {
				return nil, errors.New("invalid Git commit change list")
			}
			path, first = path[1:], false
		}
		if current == "" || path == "" {
			return nil, errors.New("invalid Git commit change list")
		}
		result[current] = append(result[current], path)
	}
	return result, nil
}

// knownCommits reports which of ids this repository has as commits, with one
// Git process.
func knownCommits(ctx context.Context, r *gitexec.Runner, root string, ids []string) (map[string]bool, error) {
	known := map[string]bool{}
	var query strings.Builder
	for _, id := range ids {
		if !allZero(id) {
			query.WriteString(id + "^{commit}\n")
		}
	}
	if query.Len() == 0 {
		return known, nil
	}
	out, err := r.GuardInput(ctx, root, strings.NewReader(query.String()), "cat-file", "--batch-check=%(objectname)")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// Missing objects print "<name> missing".
		if fields := strings.Fields(line); len(fields) == 1 && objectID.MatchString(fields[0]) {
			known[fields[0]] = true
		}
	}
	return known, nil
}

func allZero(id string) bool { return strings.Trim(id, "0") == "" }

// AuditUpstream scans local branches that declare an upstream. Doctor uses a
// full ancestry scan so an already-pushed leak is visible too.
func AuditUpstream(ctx context.Context, r *gitexec.Runner, cwd string) ([]Finding, error) {
	b, err := r.Guard(ctx, cwd, "", "for-each-ref", "--format=%(refname) %(upstream) %(objectname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	// All branches go into one scan, so shared history is read once.
	var input strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !objectID.MatchString(fields[2]) {
			continue
		}
		input.WriteString(fields[0] + " " + fields[2] + " " + fields[1] + " " + strings.Repeat("0", len(fields[2])) + "\n")
	}
	findings, err := push(ctx, r, cwd, "", strings.NewReader(input.String()), true)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := []Finding{}
	for _, finding := range findings {
		key := finding.Commit + "\x00" + finding.Path
		if !seen[key] {
			seen[key] = true
			result = append(result, finding)
		}
	}
	return result, nil
}
