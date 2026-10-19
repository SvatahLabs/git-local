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

	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// DecryptStaged authenticates an archive bound to ref and extracts it into a
// new private temporary directory. It returns that directory only after the
// terminal authentication record succeeds and the archive's repository is the
// one ref names. The caller owns removal or publication.
func DecryptStaged(ctx context.Context, encrypted io.Reader, key []byte, ref string) (string, error) {
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
		err := Decrypt(writer, encrypted, key, ref)
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
	if err := matchRefRepository(stage, ref); err != nil {
		return "", err
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
		case tar.TypeReg: // The reader reports old TypeRegA entries as TypeReg.
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

// matchRefRepository requires the staged state's repository ID to be the one
// the backup ref names.
func matchRefRepository(stage, ref string) error {
	parts := strings.Split(strings.TrimPrefix(ref, "refs/gitlocal/backups/"), "/")
	state, err := storage.OpenDir(filepath.Join(stage, "state"))
	if err != nil {
		return err
	}
	defer state.Close()
	var repository workspace.Repository
	if err := state.ReadJSON("repository.json", &repository); err != nil {
		return err
	}
	if len(parts) != 2 || repository.RepositoryID != parts[0] {
		return fmt.Errorf("backup archive belongs to repository %s, not %s named by its backup ref", repository.RepositoryID, parts[0])
	}
	return nil
}
