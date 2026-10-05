package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
)

const blobBatchFiles = 128
const blobBatchBytes = 32 << 20

var errBlobBatchTooLarge = errors.New("file exceeds blob batch limit")

type stagedBlob struct {
	path, mode, temporary string
}

// blobBatch copies already validated regular files into a private, bounded
// staging directory. Git reads only those copies with --no-filters, never a
// worktree path or a user-controlled pathname list.
type blobBatch struct {
	directory string
	items     []stagedBlob
	bytes     int64
}

func newBlobBatch() (*blobBatch, error) {
	directory, err := os.MkdirTemp("", "gitlocal-blob-batch-")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(directory) || strings.ContainsAny(directory, "\x00\r\n") {
		os.RemoveAll(directory)
		return nil, errors.New("invalid private blob staging path")
	}
	return &blobBatch{directory: directory}, nil
}

func (b *blobBatch) close() error { return os.RemoveAll(b.directory) }

func (b *blobBatch) stage(files *storage.Dir, path, mode string, expected os.FileMode) (storage.Observation, error) {
	info, err := files.Lstat(path)
	if err != nil {
		return storage.Observation{}, err
	}
	if info.Size() > blobBatchBytes {
		return storage.Observation{}, errBlobBatchTooLarge
	}
	temporary, err := os.CreateTemp(b.directory, "blob-")
	if err != nil {
		return storage.Observation{}, err
	}
	name := temporary.Name()
	observation, streamErr := files.StreamStable(path, func(input io.Reader) error {
		_, err := io.Copy(temporary, input)
		return err
	})
	closeErr := temporary.Close()
	if streamErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return storage.Observation{}, errors.Join(streamErr, closeErr)
	}
	if observation.Mode() != expected {
		_ = os.Remove(name)
		return storage.Observation{}, fmt.Errorf("%w: mode changed at %q", storage.ErrConflict, path)
	}
	if observation.Size() > blobBatchBytes {
		_ = os.Remove(name)
		return storage.Observation{}, errBlobBatchTooLarge
	}
	b.items = append(b.items, stagedBlob{path: path, mode: mode, temporary: name})
	b.bytes += observation.Size()
	return observation, nil
}

func (b *blobBatch) ready() bool { return len(b.items) >= blobBatchFiles || b.bytes >= blobBatchBytes }

func (b *blobBatch) flush(ctx context.Context, r *gitexec.Runner, store string, next map[string]fileValue) error {
	if len(b.items) == 0 {
		return nil
	}
	var paths bytes.Buffer
	for _, item := range b.items {
		paths.WriteString(item.temporary)
		paths.WriteByte('\n')
	}
	output, err := r.History(ctx, store, &paths, "hash-object", "-w", "--stdin-paths", "--no-filters")
	if err != nil {
		return err
	}
	ids := bytes.Split(output, []byte{'\n'})
	if len(ids) != len(b.items)+1 || len(ids[len(ids)-1]) != 0 {
		return errors.New("invalid batched blob IDs")
	}
	for i, item := range b.items {
		id := string(ids[i])
		if !validOID(id) {
			return errors.New("invalid batched blob ID")
		}
		next[item.path] = fileValue{item.mode, id}
		if err := os.Remove(item.temporary); err != nil {
			return err
		}
	}
	b.items = nil
	b.bytes = 0
	return nil
}
