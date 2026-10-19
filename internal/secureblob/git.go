package secureblob

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/SvatahLabs/git-local/internal/gitexec"
)

// EncryptGitBlob writes only authenticated ciphertext to the durable private
// Git object store. It returns the ciphertext object ID; plaintext is never
// passed to hash-object.
func EncryptGitBlob(ctx context.Context, r *gitexec.Runner, store string, src io.Reader, key []byte, id Identity) (string, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := Encrypt(writer, src, key, id)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	b, hashErr := r.History(ctx, store, reader, "hash-object", "-w", "--stdin", "--no-filters")
	_ = reader.Close()
	encryptErr := <-done
	if encryptErr != nil {
		return "", encryptErr
	}
	if hashErr != nil {
		return "", hashErr
	}
	objectID := strings.TrimSpace(string(b))
	if len(objectID) != 40 && len(objectID) != 64 {
		return "", errors.New("invalid encrypted Git blob ID")
	}
	for _, c := range objectID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", errors.New("invalid encrypted Git blob ID")
		}
	}
	return objectID, nil
}

// DecryptGitBlob streams one ciphertext Git blob through authenticated
// decryption. The caller must stage dst until this returns successfully because
// earlier chunks can have been written before a later authentication failure.
func DecryptGitBlob(ctx context.Context, r *gitexec.Runner, store, objectID string, dst io.Writer, key []byte, id Identity) error {
	return decryptFrom(func(w io.Writer) error { return r.HistoryBlobTo(ctx, store, objectID, w) }, dst, key, id)
}

// DecryptBlob is DecryptGitBlob reading the ciphertext through blobs, so
// many blobs share one Git process.
func DecryptBlob(blobs *gitexec.BlobReader, objectID string, dst io.Writer, key []byte, id Identity) error {
	return decryptFrom(func(w io.Writer) error { return blobs.Blob(objectID, w) }, dst, key, id)
}

func decryptFrom(read func(io.Writer) error, dst io.Writer, key []byte, id Identity) error {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := read(writer)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	decryptErr := Decrypt(dst, reader, key, id)
	_ = reader.Close()
	readErr := <-done
	if decryptErr != nil {
		return decryptErr
	}
	return readErr
}
