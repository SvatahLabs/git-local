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

// PackEncrypted streams Git Local's common state and the source checkout's
// identity through the authenticated cipher. The caller must hold the common
// repository lock and reject pending recovery transactions first.
func PackEncrypted(ctx context.Context, stateRoot, admin string, dst io.Writer, key []byte) error {
	state, err := storage.OpenDir(stateRoot)
	if err != nil {
		return err
	}
	defer state.Close()
	adminDir, err := storage.OpenDir(admin)
	if err != nil {
		return err
	}
	defer adminDir.Close()
	identity, err := adminDir.ReadFile("gitlocal-workspace.json", 64<<10)
	if err != nil {
		return err
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		tarWriter := tar.NewWriter(writer)
		err := tarWriter.WriteHeader(&tar.Header{Name: "identity.json", Mode: 0600, Size: int64(len(identity)), Typeflag: tar.TypeReg})
		if err == nil {
			err = writeFull(tarWriter, identity)
		}
		if err == nil {
			err = packDirectory(ctx, state, tarWriter, "", "state")
		}
		if closeErr := tarWriter.Close(); err == nil {
			err = closeErr
		}
		writer.CloseWithError(err)
		done <- err
	}()
	encErr := Encrypt(dst, reader, key)
	if encErr != nil {
		reader.CloseWithError(encErr)
	}
	packErr := <-done
	reader.Close()
	if encErr != nil {
		return encErr
	}
	return packErr
}

func packDirectory(ctx context.Context, root *storage.Dir, dst *tar.Writer, rel, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dst.WriteHeader(&tar.Header{Name: name + "/", Mode: 0700, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	entries, err := root.ReadDir(rel)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		component := entry.Name()
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, "/\\\x00") {
			return errors.New("invalid backup state path")
		}
		child := component
		if rel != "" {
			child = filepath.Join(rel, component)
		}
		// The quarantine holds local rescue copies, possibly plaintext; it
		// isn't private history and stays out of backups.
		if filepath.ToSlash(child) == "locks/repository.lock" || child == "quarantine" {
			continue
		}
		info, err := root.Lstat(child)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinked backup state %q", child)
		}
		archiveName := path.Join("state", filepath.ToSlash(child))
		if info.IsDir() {
			if err := packDirectory(ctx, root, dst, child, archiveName); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return fmt.Errorf("unsupported backup state file %q", child)
		}
		if err := dst.WriteHeader(&tar.Header{Name: archiveName, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = root.StreamStable(child, func(input io.Reader) error {
			_, copyErr := io.Copy(dst, input)
			return copyErr
		})
		if err != nil {
			return err
		}
	}
	return nil
}
