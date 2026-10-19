package overlay

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/storage"
)

type Status struct {
	Name, Path     string
	Version        uint32
	Working, Index string
	UpstreamRef    string
	UpstreamBlob   string
	Upstream       string
	// Adopted counts the patch's Hunks that the upstream version of the file
	// already carries, for example because a teammate committed the same
	// change. Such an overlay is stale and should be saved again.
	Adopted, Hunks int
}

// adoptedHunks counts the hunks of v that the committed version already
// carries, out of all of them.
func adoptedHunks(ctx context.Context, git string, v Version, committed []byte) (int, int, error) {
	versions, err := hunkVersions(ctx, git, v.Base, v.Local)
	if err != nil {
		return 0, 0, err
	}
	if len(versions) == 0 {
		versions = [][]byte{v.Local}
	}
	if validateText(committed) != nil {
		return 0, len(versions), nil
	}
	adopted := 0
	for _, local := range versions {
		// A version the hunk conflicts with doesn't carry it.
		if applied, err := Apply(ctx, git, v.Base, local, committed); err == nil && bytes.Equal(applied, committed) {
			adopted++
		}
	}
	return adopted, len(versions), nil
}

func activeVersions(ctx context.Context, commonPath, id string) ([]Version, error) {
	common, err := storage.OpenDir(commonPath)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	lock, err := common.LockExisting(ctx, "gitlocal/locks/repository.lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	d, err := storage.OpenDir(filepath.Join(commonPath, "gitlocal"))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(filepath.Join("overlays", id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	versions := make([]Version, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validName.MatchString(entry.Name()) {
			return nil, errors.New("invalid overlay registration")
		}
		name := entry.Name()
		var m Manifest
		if err := d.ReadJSON(filepath.Join(prefix(id, name), "manifest.json"), &m); err != nil {
			return nil, err
		}
		if err := checkManifest(m, id, name); err != nil {
			return nil, err
		}
		if m.State == "removed" {
			continue
		}
		v, err := readVersion(d, m, m.Version)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, nil
}

func upstreamRef(ctx context.Context, r *gitexec.Runner, root string) (string, error) {
	b, err := r.Guard(ctx, root, "", "rev-parse", "--symbolic-full-name", "HEAD")
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(b))
	if !strings.HasPrefix(head, "refs/heads/") || strings.ContainsAny(head, "\x00\r\n") {
		return "", nil
	}
	b, err = r.Guard(ctx, root, "", "for-each-ref", "--format=%(upstream)", head)
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(string(b))
	if ref == "" {
		return "", nil
	}
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\x00\r\n") {
		return "", errors.New("invalid upstream ref")
	}
	return ref, nil
}

// Inspect reports whether each active patch can be applied to the current
// index and the configured upstream branch. It does not modify any Git state.
func Inspect(ctx context.Context, r *gitexec.Runner, cwd string) ([]Status, error) {
	p, err := NewProcessor(ctx, r, cwd, "")
	if err != nil {
		return nil, err
	}
	if p.WorkspaceID == "" {
		return nil, errors.New("worktree needs git local init before inspecting overlays")
	}
	versions, err := activeVersions(ctx, p.Common, p.WorkspaceID)
	if err != nil || len(versions) == 0 {
		return nil, err
	}
	upstream, err := upstreamRef(ctx, r, p.Root)
	if err != nil {
		return nil, err
	}
	files, err := storage.OpenDir(p.Root)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	result := make([]Status, 0, len(versions))
	for _, v := range versions {
		s := Status{Name: v.Manifest.Name, Path: v.Manifest.Path, Version: v.Manifest.Version, UpstreamRef: upstream}
		if v.Manifest.State == "removing" {
			s.Index = "removal pending"
		} else if blob, err := r.IndexBlob(ctx, p.Root, "", s.Path); err != nil {
			s.Index = "unavailable"
		} else if _, err := Apply(ctx, r.Executable(), v.Base, v.Local, blob); errors.Is(err, ErrConflict) {
			s.Index = "conflict"
		} else if err != nil {
			return nil, err
		} else {
			s.Index = "ready"
		}
		info, err := files.Lstat(filepath.FromSlash(s.Path))
		if os.IsNotExist(err) {
			s.Working = "missing"
		} else if err != nil {
			return nil, err
		} else if info.Mode().IsRegular() {
			s.Working = "present"
		} else {
			s.Working = "non-regular"
		}
		if upstream != "" {
			b, err := r.Guard(ctx, p.Root, "", "rev-parse", "--verify", upstream+":"+s.Path)
			if err != nil {
				s.Upstream = "path unavailable"
			} else {
				s.UpstreamBlob = strings.TrimSpace(string(b))
				blob, err := r.Guard(ctx, p.Root, "", "cat-file", "blob", s.UpstreamBlob)
				if err != nil {
					return nil, err
				}
				if _, err := Apply(ctx, r.Executable(), v.Base, v.Local, blob); errors.Is(err, ErrConflict) {
					s.Upstream = "conflict"
				} else if err != nil {
					return nil, err
				} else {
					s.Upstream = "ready"
				}
				if s.Adopted, s.Hunks, err = adoptedHunks(ctx, r.Executable(), v, blob); err != nil {
					return nil, err
				}
			}
		}
		result = append(result, s)
	}
	return result, nil
}
