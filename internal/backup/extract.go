package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitlocal/internal/storage"
)

// DecryptStaged authenticates an archive and extracts it into a new private
// temporary directory. It returns that directory only after the terminal
// authentication record succeeds. The caller owns removal or publication.
func DecryptStaged(ctx context.Context, encrypted io.Reader, key []byte) (string, error) {
	stage, err := os.MkdirTemp("", "gitlocal-restore-stage-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(stage)
		}
	}()
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := Decrypt(writer, encrypted, key)
		writer.CloseWithError(err)
		done <- err
	}()
	extractErr := extractTar(ctx, stage, reader)
	if extractErr != nil {
		reader.CloseWithError(extractErr)
	}
	decryptErr := <-done
	reader.Close()
	if extractErr != nil {
		return "", extractErr
	}
	if decryptErr != nil {
		return "", decryptErr
	}
	success = true
	return stage, nil
}

func extractTar(ctx context.Context, stage string, src io.Reader) error {
	root, err := storage.OpenDir(stage)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(src)
	identity, state, entries := false, false, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > 1000000 {
			return errors.New("backup archive has too many entries")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00\r\n") || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("unsafe backup archive path %q", header.Name)
		}
		if name != "identity.json" && name != "state" && !strings.HasPrefix(name, "state/") {
			return fmt.Errorf("unexpected backup archive path %q", header.Name)
		}
		local := filepath.FromSlash(name)
		switch header.Typeflag {
		case tar.TypeDir:
			if name == "identity.json" || header.Size != 0 {
				return errors.New("invalid backup archive directory")
			}
			if err := root.MakeDirectory(local); err != nil {
				return err
			}
			if name == "state" {
				state = true
			}
		case tar.TypeReg, tar.TypeRegA:
			if name == "state" || header.Size < 0 || name == "identity.json" && header.Size > 64<<10 {
				return errors.New("invalid backup archive file")
			}
			mode := os.FileMode(0600)
			if header.Mode&0111 != 0 {
				mode = 0700
			}
			if err := root.StageNamed(local, mode, func(dst io.Writer) error {
				_, err := io.CopyN(dst, reader, header.Size)
				return err
			}); err != nil {
				return err
			}
			if name == "identity.json" {
				identity = true
			}
		default:
			return errors.New("backup archive contains unsupported file type")
		}
	}
	if !identity || !state {
		return errors.New("backup archive lacks identity or private state")
	}
	_, err = io.Copy(io.Discard, src)
	return err
}
