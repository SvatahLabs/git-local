package overlay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

type Processor struct {
	Runner                           *gitexec.Runner
	Root, Common, WorkspaceID, Index string
}

func resolveGitPath(ctx context.Context, r *gitexec.Runner, cwd string, args ...string) (string, error) {
	b, err := r.Guard(ctx, cwd, "", append([]string{"rev-parse"}, args...)...)
	if err != nil {
		return "", err
	}
	p := strings.TrimSuffix(string(b), "\n")
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n") {
		return "", errors.New("invalid Git administrative path")
	}
	return filepath.Clean(p), nil
}

// NewProcessor binds a long-running Git filter to one initialized worktree.
// The active index may be Git's temporary commit index.
func NewProcessor(ctx context.Context, r *gitexec.Runner, cwd, index string) (Processor, error) {
	var p Processor
	p.Runner, p.Index = r, index
	root, err := resolveGitPath(ctx, r, cwd, "--show-toplevel")
	if err != nil {
		return p, err
	}
	common, err := resolveGitPath(ctx, r, cwd, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return p, err
	}
	admin, err := resolveGitPath(ctx, r, cwd, "--absolute-git-dir")
	if err != nil {
		return p, err
	}
	d, err := storage.OpenDir(admin)
	if err != nil {
		return p, err
	}
	defer d.Close()
	var identity workspace.Identity
	identityErr := d.ReadJSON("gitlocal-workspace.json", &identity)
	if identityErr != nil && !os.IsNotExist(identityErr) {
		return p, fmt.Errorf("read overlay worktree identity: %w", identityErr)
	}
	if identityErr == nil && (identity.SchemaVersion != workspace.SchemaVersion || !storage.ValidUUID(identity.WorkspaceID) || !storage.ValidUUID(identity.RepositoryID)) {
		return p, errors.New("invalid overlay worktree identity")
	}
	commonDir, err := storage.OpenDir(common)
	if err != nil {
		return p, err
	}
	defer commonDir.Close()
	var repository workspace.Repository
	if err := commonDir.ReadJSON("gitlocal/repository.json", &repository); err != nil {
		return p, err
	}
	if repository.SchemaVersion != workspace.SchemaVersion || !storage.ValidUUID(repository.RepositoryID) || (identityErr == nil && repository.RepositoryID != identity.RepositoryID) {
		return p, errors.New("overlay worktree belongs to a different repository")
	}
	if identityErr != nil {
		entries, err := commonDir.ReadDir("gitlocal/workspaces")
		if err != nil {
			return p, err
		}
		for _, entry := range entries {
			if !entry.IsDir() || !storage.ValidUUID(entry.Name()) {
				return p, errors.New("invalid overlay workspace registration")
			}
			var record workspace.Record
			if err := commonDir.ReadJSON(filepath.Join("gitlocal/workspaces", entry.Name(), "workspace.json"), &record); err != nil {
				return p, err
			}
			if record.SchemaVersion != workspace.SchemaVersion || record.WorkspaceID != entry.Name() {
				return p, errors.New("invalid overlay workspace record")
			}
			if record.Admin == admin && record.State == "active" {
				return p, errors.New("registered overlay worktree identity is missing")
			}
		}
	}
	p.Root, p.Common, p.WorkspaceID = root, common, identity.WorkspaceID
	return p, nil
}

func (p Processor) Transform(ctx context.Context, command, path string, input []byte) ([]byte, error) {
	if storage.ValidateSelectionPath(path) != nil || strings.Contains(path, "\\") {
		return nil, errors.New("invalid overlay path")
	}
	version, found, owned, err := LoadPath(ctx, p.Common, p.WorkspaceID, path)
	if err != nil {
		return nil, err
	}
	if !found {
		if !owned {
			return nil, errors.New("overlay filter has no saved patch for this path; rerun git local overlay add")
		}
		return input, nil
	}
	if err := validateText(input); err != nil {
		return nil, err
	}
	switch command {
	case "smudge":
		return Apply(ctx, p.Runner.Executable(), version.Base, version.Local, input)
	case "clean":
		indexBlob, err := p.Runner.IndexBlob(ctx, p.Root, p.Index, path)
		if err != nil {
			return nil, fmt.Errorf("read current index blob for %q: %w", path, err)
		}
		applied, err := Apply(ctx, p.Runner.Executable(), version.Base, version.Local, indexBlob)
		if err != nil {
			return nil, err
		}
		return Strip(ctx, p.Runner.Executable(), indexBlob, applied, input)
	default:
		return nil, errors.New("unsupported overlay filter command")
	}
}

// IndexFromEnvironment returns only Git's alternate-index path. Repository and
// configuration selection variables remain isolated in the runner.
func IndexFromEnvironment() string { return os.Getenv("GIT_INDEX_FILE") }
