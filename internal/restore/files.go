package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/storage"
)

func inspectFile(ctx context.Context, r *gitexec.Runner, store string, files *storage.Dir, path string) (Fingerprint, error) {
	fi, err := files.Lstat(path)
	if os.IsNotExist(err) {
		return Fingerprint{}, nil
	}
	if err != nil {
		return Fingerprint{}, err
	}
	before := Fingerprint{Exists: true, Kind: "file", GitMode: "100644", Permissions: uint32(fi.Mode().Perm())}
	sum := sha256.New()
	var id []byte
	if fi.Mode()&os.ModeSymlink != 0 {
		target, e := files.ReadLink(path)
		if e != nil {
			return before, e
		}
		before.Kind = "symlink"
		before.GitMode = "120000"
		before.Size = int64(len(target))
		sum.Write([]byte(target))
		id, err = r.History(ctx, store, strings.NewReader(target), "hash-object", "--stdin", "--no-filters")
		again, e := files.ReadLink(path)
		if e != nil || again != target {
			return before, fmt.Errorf("%w: link changed during planning at %q", storage.ErrConflict, path)
		}
	} else {
		if !fi.Mode().IsRegular() {
			return before, fmt.Errorf("%w: directory or unsupported type obstructs %q", storage.ErrConflict, path)
		}
		var observation storage.Observation
		observation, err = files.StreamStable(path, func(input io.Reader) error {
			var e error
			id, e = r.History(ctx, store, io.TeeReader(input, sum), "hash-object", "--stdin", "--no-filters")
			return e
		})
		if err == nil {
			before.Size = observation.Size()
			before.Permissions = uint32(observation.Mode().Perm())
			if observation.Mode()&0111 != 0 {
				before.GitMode = "100755"
			}
		}
	}
	if err != nil {
		return before, err
	}
	before.BlobID = strings.TrimSpace(string(id))
	if len(before.BlobID) != 40 && len(before.BlobID) != 64 {
		return before, fmt.Errorf("invalid file object ID")
	}
	for _, c := range before.BlobID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return before, fmt.Errorf("invalid file object ID")
		}
	}
	before.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return before, nil
}
func matches(file Fingerprint, value history.SnapshotFile, exists bool) bool {
	if !exists {
		return !file.Exists
	}
	return file.Exists && file.BlobID == value.ID && file.GitMode == value.Mode
}
func restorePermissions(before Fingerprint, mode string) uint32 {
	if mode == "120000" {
		return 0
	}
	permissions := uint32(0600)
	if before.Exists && before.Kind == "file" {
		permissions = before.Permissions & 0666
	}
	if mode == "100755" {
		execute := before.Permissions & 0111
		if before.Kind != "file" || execute == 0 {
			execute = 0100
		}
		permissions |= execute
	}
	return permissions
}
func validateSourceMode(ctx context.Context, r *gitexec.Runner, store string, file history.SnapshotFile) error {
	if runtime.GOOS == "windows" && (file.Mode == "120000" || file.Mode == "100755") {
		return fmt.Errorf("unsupported source mode %s for Windows restore planning", file.Mode)
	}
	if file.Mode == "120000" {
		sizeBytes, err := r.History(ctx, store, nil, "cat-file", "-s", file.ID)
		if err != nil {
			return err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(sizeBytes)), 10, 64)
		if err != nil || size < 1 || size > 1<<20 {
			return fmt.Errorf("invalid or unsupported source symlink size")
		}
		data, err := r.History(ctx, store, nil, "cat-file", "blob", file.ID)
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data) > 1<<20 || strings.ContainsRune(string(data), 0) {
			return fmt.Errorf("invalid or unsupported source symlink target")
		}
	}
	return nil
}
