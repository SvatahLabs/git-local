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

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func ValidateBackupRef(ref string) error {
	parts := strings.Split(strings.TrimPrefix(ref, "refs/gitlocal/backups/"), "/")
	if !strings.HasPrefix(ref, "refs/gitlocal/backups/") || len(parts) != 2 || !storage.ValidUUID(parts[0]) || len(parts[1]) != 16+1+36 || parts[1][16] != '-' || !storage.ValidUUID(parts[1][17:]) {
		return errors.New("invalid Git Local backup ref")
	}
	if _, err := time.Parse("20060102T150405Z", parts[1][:16]); err != nil {
		return errors.New("invalid Git Local backup ref timestamp")
	}
	return nil
}

func EffectiveSource(ctx context.Context, r *gitexec.Runner, w workspace.Context, name string) (string, error) {
	if !validRemoteName(name) || name == "origin" {
		return "", errors.New("choose a named backup remote other than origin")
	}
	source, err := effectiveURLs(ctx, r, w, name, false)
	if err != nil {
		return "", err
	}
	if len(source) != 1 {
		return "", errors.New("backup restore requires exactly one effective fetch URL")
	}
	originFetch, err := effectiveURLs(ctx, r, w, "origin", false)
	if err != nil {
		return "", fmt.Errorf("origin is required to verify the backup host: %w", err)
	}
	originPush, err := effectiveURLs(ctx, r, w, "origin", true)
	if err != nil {
		return "", err
	}
	if err := ValidateDestination(name, source, append(originFetch, originPush...)); err != nil {
		return "", err
	}
	return source[0], nil
}

type fetchTransport func(context.Context, *gitexec.Runner, string, string, string) error

// FetchStaged fetches one encrypted backup without touching mainline Git or
// the current Git Local state. The returned staged directory is authenticated
// and still private; the caller must validate it before publication.
func FetchStaged(ctx context.Context, r *gitexec.Runner, cwd, remote, ref, keyFile string) (string, error) {
	return fetchStaged(ctx, r, cwd, remote, ref, keyFile, func(ctx context.Context, r *gitexec.Runner, store, url, ref string) error {
		return r.BackupFetch(ctx, store, url, ref)
	})
}

func fetchStaged(ctx context.Context, r *gitexec.Runner, cwd, remote, ref, keyFile string, transport fetchTransport) (string, error) {
	if err := ValidateBackupRef(ref); err != nil {
		return "", err
	}
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return "", err
	}
	key, err := LoadKeyFile(keyFile, w.Root)
	if err != nil {
		return "", err
	}
	url, err := EffectiveSource(ctx, r, *w, remote)
	if err != nil {
		return "", err
	}
	temporary, err := os.MkdirTemp("", "gitlocal-backup-fetch-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	store := filepath.Join(temporary, "fetch.git")
	if err := r.InitBare(ctx, store); err != nil {
		return "", err
	}
	if err := transport(ctx, r, store, url, ref); err != nil {
		return "", err
	}
	commit, err := r.History(ctx, store, nil, "rev-parse", "--verify", "refs/heads/import")
	if err != nil {
		return "", err
	}
	entries, err := r.History(ctx, store, nil, "ls-tree", "-r", "-z", strings.TrimSpace(string(commit)))
	if err != nil {
		return "", err
	}
	records := bytes.Split(entries, []byte{0})
	if len(records) != 2 || len(records[1]) != 0 {
		return "", errors.New("backup commit must contain exactly one encrypted archive")
	}
	header, name, ok := strings.Cut(string(records[0]), "\t")
	fields := strings.Fields(header)
	if !ok || name != "archive.enc" || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" {
		return "", errors.New("backup commit has an invalid archive entry")
	}
	encrypted, err := os.OpenFile(filepath.Join(temporary, "archive.enc"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if err = r.HistoryBlobTo(ctx, store, fields[2], encrypted); err != nil {
		encrypted.Close()
		return "", err
	}
	if err = encrypted.Sync(); err != nil {
		encrypted.Close()
		return "", err
	}
	if err = encrypted.Close(); err != nil {
		return "", err
	}
	input, err := os.Open(filepath.Join(temporary, "archive.enc"))
	if err != nil {
		return "", err
	}
	defer input.Close()
	return DecryptStaged(ctx, input, key, ref)
}
