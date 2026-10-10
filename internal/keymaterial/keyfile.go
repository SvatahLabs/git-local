// Package keymaterial loads owner-private 256-bit keys kept outside a checkout.
package keymaterial

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gitlocal/internal/storage"
)

func keyPath(path, repoRoot string, existing bool) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("key path must be absolute")
	}
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(parent, filepath.Base(path))
	if existing {
		info, err := os.Lstat(resolved)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("key must be a regular file, not a symlink")
		}
	}
	if inside, err := Inside(root, resolved); err != nil {
		return "", err
	} else if inside {
		return "", errors.New("key must be stored outside the repository checkout")
	}
	return resolved, nil
}

// Inside reports whether path is root or lies below it. It compares
// directories by file identity rather than spelling, so on a case-insensitive
// or normalizing volume a differently spelled path into root still counts.
// path itself need not exist.
func Inside(root, path string) (bool, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	for dir := filepath.Clean(path); ; {
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, rootInfo) {
			return true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}

// Create writes a new random 32-byte key with owner-only permissions. It
// refuses existing files, symlinked key files and paths inside the checkout.
func Create(path, repoRoot string) error {
	path, err := keyPath(path, repoRoot, false)
	if err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if n, err := file.Write(key); err != nil {
		return err
	} else if n != len(key) {
		return errors.New("short key write")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

// Load accepts only a 32-byte owner-private regular file outside the checkout.
func Load(path, repoRoot string) ([]byte, error) {
	path, err := keyPath(path, repoRoot, true)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 32 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("key must be a 32-byte owner-private regular file")
	}
	dir, err := storage.OpenDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	data, err := dir.ReadFile(filepath.Base(path), 33)
	if err != nil {
		return nil, err
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("key changed while reading: got %d bytes", len(data))
	}
	return data, nil
}

// Fingerprint identifies a key without putting key bytes in repository state.
// It uses a domain-specific digest so it cannot be confused with a file hash.
func Fingerprint(key []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("key must be exactly 32 bytes")
	}
	data := append([]byte("git-local key fingerprint v1\x00"), key...)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
