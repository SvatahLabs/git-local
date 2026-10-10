package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/overlay"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type PushResult struct {
	Ref          string
	Destinations int
}

type pushTransport func(context.Context, *gitexec.Runner, string, string, string, string) error

// Push encrypts a consistent local-state archive and pushes one isolated
// commit per effective URL. The original repository's refs and objects are
// never used as the source of the push.
func Push(ctx context.Context, r *gitexec.Runner, cwd, remote, keyFile string) (PushResult, error) {
	return push(ctx, r, cwd, remote, keyFile, func(ctx context.Context, r *gitexec.Runner, store, url, commit, ref string) error {
		return r.BackupPush(ctx, store, url, commit, ref)
	})
}

func push(ctx context.Context, r *gitexec.Runner, cwd, remote, keyFile string, transport pushTransport) (result PushResult, err error) {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp("", "gitlocal-backup-stage-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	store := filepath.Join(stage, "archive.git")
	if err := r.InitBare(ctx, store); err != nil {
		return result, err
	}
	if err := CheckKeyLocation(ctx, r, *w, keyFile); err != nil {
		return result, err
	}
	key, err := LoadKeyFile(keyFile, w.Root)
	if err != nil {
		return result, err
	}
	archive, err := os.OpenFile(filepath.Join(stage, "archive.enc"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer archive.Close()
	var urls []string
	var ref string
	nonce, err := storage.UUID()
	if err != nil {
		return result, err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	err = configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		var e error
		state, e := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
		if e != nil {
			return e
		}
		_, e = overlay.ValidateBackupRecords(state)
		state.Close()
		if e != nil {
			return e
		}
		urls, e = EffectiveDestination(ctx, r, w, remote)
		if e != nil {
			return e
		}
		ref = fmt.Sprintf("refs/gitlocal/backups/%s/%s-%s", w.RepositoryID, stamp, nonce)
		if e = PackEncrypted(ctx, filepath.Join(w.Common, "gitlocal"), w.Admin, archive, key, ref); e != nil {
			return e
		}
		return recheck()
	})
	if err != nil {
		return result, err
	}
	if err = archive.Sync(); err != nil {
		return result, err
	}
	if _, err = archive.Seek(0, 0); err != nil {
		return result, err
	}
	blob, err := r.History(ctx, store, archive, "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		return result, err
	}
	blobID := strings.TrimSpace(string(blob))
	if len(blobID) != 40 {
		return result, errors.New("invalid encrypted archive object ID")
	}
	treeInput := fmt.Sprintf("100644 blob %s\tarchive.enc%c", blobID, 0)
	tree, err := r.History(ctx, store, strings.NewReader(treeInput), "mktree", "-z")
	if err != nil {
		return result, err
	}
	treeID := strings.TrimSpace(string(tree))
	commit, err := r.History(ctx, store, bytes.NewBufferString("Encrypted Git Local backup\n"), "commit-tree", treeID)
	if err != nil {
		return result, err
	}
	commitID := strings.TrimSpace(string(commit))
	result.Ref = ref
	for _, url := range urls {
		if err := transport(ctx, r, store, url, commitID, result.Ref); err != nil {
			return result, err
		}
		result.Destinations++
	}
	return result, nil
}
