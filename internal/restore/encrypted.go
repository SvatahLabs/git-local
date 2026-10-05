package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

// sensitiveImage is the evidence safe to persist for a protected worktree
// file. Its digest is keyed and identity-bound; it never contains a plain Git
// object ID or an unkeyed content hash.
type sensitiveImage struct {
	Exists      bool   `json:"exists"`
	Kind        string `json:"kind,omitempty"`
	GitMode     string `json:"gitMode,omitempty"`
	Digest      string `json:"digest,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Permissions uint32 `json:"permissions,omitempty"`
}

func protectedFingerprint(image sensitiveImage) Fingerprint {
	return Fingerprint{Exists: image.Exists, Kind: image.Kind, GitMode: image.GitMode, KeyedDigest: image.Digest, Size: image.Size, Permissions: image.Permissions}
}

func protectedBlobFingerprint(ctx context.Context, r *gitexec.Runner, store, blob string, key []byte, identity secureblob.Identity, permissions uint32) (Fingerprint, error) {
	digest, size, err := encryptedBlobDigest(ctx, r, store, blob, key, identity)
	if err != nil {
		return Fingerprint{}, err
	}
	image := Fingerprint{Exists: true, Kind: "file", GitMode: identity.Mode, KeyedDigest: digest, Size: size, Permissions: permissions}
	if identity.Mode == "120000" {
		image.Kind = "symlink"
		image.Permissions = 0777
	}
	return image, nil
}

func matchesSensitiveFile(files *storage.Dir, path string, key []byte, identity secureblob.Identity, digest string, exists bool) (bool, error) {
	image, err := inspectSensitiveFile(files, path, key, identity)
	if err != nil {
		return false, err
	}
	if !exists {
		return !image.Exists, nil
	}
	return image.Exists && image.GitMode == identity.Mode && image.Digest == digest, nil
}

func inspectSensitiveFile(files *storage.Dir, path string, key []byte, identity secureblob.Identity) (sensitiveImage, error) {
	info, err := files.Lstat(path)
	if os.IsNotExist(err) {
		return sensitiveImage{}, nil
	}
	if err != nil {
		return sensitiveImage{}, err
	}
	image := sensitiveImage{Exists: true, Kind: "file", GitMode: "100644", Permissions: uint32(info.Mode().Perm())}
	if info.Mode()&os.ModeSymlink != 0 {
		target, e := files.ReadLink(path)
		if e != nil {
			return sensitiveImage{}, e
		}
		image.Kind, image.GitMode, image.Size, image.Permissions = "symlink", "120000", int64(len(target)), 0777
		image.Digest, err = secureblob.Digest(bytes.NewReader([]byte(target)), key, identity)
		again, e := files.ReadLink(path)
		if e != nil || again != target {
			return sensitiveImage{}, errors.New("sensitive symlink changed during inspection")
		}
	} else {
		if !info.Mode().IsRegular() {
			return sensitiveImage{}, errors.New("unsupported sensitive worktree file")
		}
		var observation storage.Observation
		observation, err = files.StreamStable(path, func(input io.Reader) error {
			var e error
			image.Digest, e = secureblob.Digest(input, key, identity)
			return e
		})
		if err == nil {
			image.Size = observation.Size()
			image.Permissions = uint32(observation.Mode().Perm())
			if observation.Mode()&0111 != 0 {
				image.GitMode = "100755"
			}
		}
	}
	if err != nil {
		return sensitiveImage{}, err
	}
	return image, nil
}

// encryptedBlobDigest authenticates a stored ciphertext blob and computes the
// same keyed digest that inspectSensitiveFile computes for its plaintext.
func encryptedBlobDigest(ctx context.Context, r *gitexec.Runner, store, blob string, key []byte, identity secureblob.Identity) (string, int64, error) {
	reader, writer := io.Pipe()
	var count countWriter
	var validator io.Writer = io.Discard
	if identity.Mode == "120000" {
		validator = &symlinkContentValidator{}
	}
	done := make(chan error, 1)
	go func() {
		err := secureblob.DecryptGitBlob(ctx, r, store, blob, io.MultiWriter(validator, writer, &count), key, identity)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	digest, digestErr := secureblob.Digest(reader, key, identity)
	_ = reader.Close()
	decryptErr := <-done
	if decryptErr != nil {
		return "", 0, decryptErr
	}
	if digestErr != nil {
		return "", 0, digestErr
	}
	if identity.Mode == "120000" && count == 0 {
		return "", 0, errors.New("empty encrypted symlink target")
	}
	return digest, int64(count), nil
}

func loadRestoreKey(w workspace.Context) (secureblob.Policy, []byte, error) {
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return secureblob.Policy{}, nil, err
	}
	defer common.Close()
	policy, exists, err := secureblob.ReadLocked(common, w.Root, w.Common)
	if err != nil {
		return secureblob.Policy{}, nil, err
	}
	if !exists {
		return secureblob.Policy{}, nil, nil
	}
	key, err := secureblob.LoadKey(policy, w.Root, w.Common)
	return policy, key, err
}

type symlinkContentValidator struct{ count int64 }

func (v *symlinkContentValidator) Write(data []byte) (int, error) {
	if v.count+int64(len(data)) > 1<<20 || bytes.IndexByte(data, 0) >= 0 {
		return 0, errors.New("invalid or unsupported encrypted symlink target")
	}
	v.count += int64(len(data))
	return len(data), nil
}
