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

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/storage"
)

// PackEncrypted streams Git Local's common state and the source checkout's
// identity through the authenticated cipher, bound to the backup ref. The
// caller must hold the common repository lock and reject pending recovery
// transactions first.
func PackEncrypted(ctx context.Context, stateRoot, admin string, dst io.Writer, key []byte, ref string) error {
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
	encErr := Encrypt(dst, reader, key, ref)
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
		// isn't private history and stays out of backups. A temporary file
		// left by an interrupted atomic write is never state.
		// The stat cache (history.StatCacheDir) only saves reads on this machine (GL-138).
		if filepath.ToSlash(child) == "locks/repository.lock" || child == "quarantine" || child == "statcache" || storage.IsTempName(component) {
			continue
		}
		// A file list kept beside a seed journal is state only while its
		// completed journal exists; one left by an interrupted seed or an
		// abandoned migration isn't needed to restore, and restore refuses it,
		// as it refuses anything else there (a copy of a list, say).
		if filepath.ToSlash(rel) == "transactions/seed-files" {
			id, ok := strings.CutSuffix(component, ".json")
			if !ok || !storage.ValidUUID(id) || !entry.Type().IsRegular() {
				continue
			}
			if _, err := root.Lstat(filepath.Join("transactions", "completed", "seed-"+id+".json")); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return err
			}
		}
		// Restore reads a completed seed journal with its file list, so a
		// lost list would make the backup unrestorable.
		if filepath.ToSlash(rel) == "transactions/completed" && strings.HasPrefix(component, "seed-") && strings.HasSuffix(component, ".json") {
			if err := configtx.CheckSeedList(root, child); err != nil {
				return err
			}
		}
		// Restore accepts only known state, so anything else would make the
		// backup unrestorable.
		if rel == "" && !stateEntries[component] {
			return fmt.Errorf("%w: private state %q isn't part of a backup, and restore would refuse it; it may be left over from an interrupted encryption migration (git local recover --list shows whether one is pending); if nothing is pending, move it out of the gitlocal directory in the common Git directory", storage.ErrConflict, component)
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
