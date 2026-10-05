// Package secureblob defines the authenticated format for sensitive snapshot
// file content. The caller keeps the master key outside the checkout.
package secureblob

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

const ChunkSize = 1 << 20

var magic = []byte("GITLOCAL-FILE\x01")

// Identity binds ciphertext to its repository, workspace, path and Git mode.
// Moving a blob to another file or changing its mode fails authentication.
type Identity struct {
	RepositoryID string
	WorkspaceID  string
	Path         string
	Mode         string
}

func (id Identity) validate() error {
	if id.RepositoryID == "" || id.WorkspaceID == "" || id.Path == "" {
		return errors.New("encrypted file identity is incomplete")
	}
	if id.Mode != "100644" && id.Mode != "100755" && id.Mode != "120000" {
		return errors.New("unsupported encrypted file mode")
	}
	return nil
}

func associatedData(header []byte, id Identity, counter, size uint32) []byte {
	// Length prefixes make the fields unambiguous even if a path contains a
	// separator used in another field. The terminal record uses size zero.
	fields := []string{id.RepositoryID, id.WorkspaceID, id.Path, id.Mode}
	capacity := len(header) + 8
	for _, field := range fields {
		capacity += 4 + len(field)
	}
	data := make([]byte, 0, capacity)
	data = append(data, header...)
	for _, field := range fields {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		data = append(data, length[:]...)
		data = append(data, field...)
	}
	var record [8]byte
	binary.BigEndian.PutUint32(record[:4], counter)
	binary.BigEndian.PutUint32(record[4:], size)
	return append(data, record[:]...)
}

// Digest streams a keyed content digest for no-change detection. A plain hash
// would permit offline guesses of short secrets from snapshot metadata.
func digestMAC(master []byte, id Identity) (hash.Hash, error) {
	if err := id.validate(); err != nil {
		return nil, err
	}
	if len(master) != 32 {
		return nil, errors.New("encrypted file key must be exactly 32 bytes")
	}
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("git-local sensitive file digest v1\x00"))
	for _, field := range []string{id.RepositoryID, id.WorkspaceID, id.Path, id.Mode} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write([]byte(field))
	}
	return mac, nil
}

func Digest(src io.Reader, master []byte, id Identity) (string, error) {
	mac, err := digestMAC(master, id)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(mac, src); err != nil {
		return "", err
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func aead(master, salt []byte) (cipher.AEAD, error) {
	if len(master) != 32 || len(salt) != 16 {
		return nil, errors.New("encrypted file key must be 32 bytes and salt 16 bytes")
	}
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("git-local sensitive file v1"))
	_, _ = mac.Write(salt)
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(counter uint32) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint32(n[8:], counter)
	return n
}

func writeAll(dst io.Writer, data []byte) error {
	n, err := dst.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func sealRecord(dst io.Writer, cipher cipher.AEAD, header []byte, id Identity, counter uint32, plain []byte) error {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(plain)))
	if err := writeAll(dst, size[:]); err != nil {
		return err
	}
	sealed := cipher.Seal(nil, nonce(counter), plain, associatedData(header, id, counter, uint32(len(plain))))
	return writeAll(dst, sealed)
}

// Encrypt writes independently authenticated records and an authenticated
// terminal record. It never writes plaintext to dst.
func Encrypt(dst io.Writer, src io.Reader, master []byte, id Identity) error {
	_, err := EncryptDigest(dst, src, master, id)
	return err
}

// EncryptDigest returns the keyed plaintext digest for the exact stream being
// encrypted. Ciphertext is randomized, so callers compare this digest with the
// previous snapshot and retain its blob ID when content is unchanged.
func EncryptDigest(dst io.Writer, src io.Reader, master []byte, id Identity) (string, error) {
	mac, err := digestMAC(master, id)
	if err != nil {
		return "", err
	}
	header := make([]byte, len(magic)+16)
	copy(header, magic)
	if _, err := rand.Read(header[len(magic):]); err != nil {
		return "", err
	}
	cipher, err := aead(master, header[len(magic):])
	if err != nil {
		return "", err
	}
	if err := writeAll(dst, header); err != nil {
		return "", err
	}
	buf := make([]byte, ChunkSize)
	var counter uint32
	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if counter == ^uint32(0) {
				return "", errors.New("encrypted file exceeds record limit")
			}
			_, _ = mac.Write(buf[:n])
			if err := sealRecord(dst, cipher, header, id, counter, buf[:n]); err != nil {
				return "", err
			}
			counter++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			if err := sealRecord(dst, cipher, header, id, counter, nil); err != nil {
				return "", err
			}
			return hex.EncodeToString(mac.Sum(nil)), nil
		}
		if readErr != nil {
			return "", readErr
		}
	}
}

// Decrypt authenticates every record before writing it to dst. The caller must
// stage output until this returns successfully: a later record can still fail.
func Decrypt(dst io.Writer, src io.Reader, master []byte, id Identity) error {
	if err := id.validate(); err != nil {
		return err
	}
	header := make([]byte, len(magic)+16)
	if _, err := io.ReadFull(src, header); err != nil {
		return fmt.Errorf("truncated encrypted file header: %w", err)
	}
	for i, b := range magic {
		if header[i] != b {
			return errors.New("unsupported encrypted file format")
		}
	}
	cipher, err := aead(master, header[len(magic):])
	if err != nil {
		return err
	}
	var counter uint32
	for {
		var sizeBytes [4]byte
		if _, err := io.ReadFull(src, sizeBytes[:]); err != nil {
			return fmt.Errorf("truncated encrypted file record: %w", err)
		}
		size := binary.BigEndian.Uint32(sizeBytes[:])
		if size > ChunkSize {
			return errors.New("invalid encrypted file record size")
		}
		sealed := make([]byte, int(size)+cipher.Overhead())
		if _, err := io.ReadFull(src, sealed); err != nil {
			return fmt.Errorf("truncated encrypted file ciphertext: %w", err)
		}
		plain, err := cipher.Open(nil, nonce(counter), sealed, associatedData(header, id, counter, size))
		if err != nil {
			return errors.New("encrypted file authentication failed")
		}
		if size == 0 {
			var extra [1]byte
			if n, err := src.Read(extra[:]); n != 0 || err != io.EOF {
				return errors.New("trailing data after encrypted file terminal record")
			}
			return nil
		}
		if err := writeAll(dst, plain); err != nil {
			return err
		}
		if counter == ^uint32(0) {
			return errors.New("encrypted file exceeds record limit")
		}
		counter++
	}
}
