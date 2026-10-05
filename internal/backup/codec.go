// Package backup owns the encrypted archive format used for opt-in remote
// backups. A backup key is 32 random bytes supplied separately from the data.
package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const chunkSize = 1 << 20

var archiveMagic = []byte("GITLOCAL-BACKUP\x01")

// GenerateKey creates a raw AES-256 master key. Callers must keep it outside
// the clone; losing the clone and its key must not make a remote backup useless.
func GenerateKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	return key, err
}

func archiveAEAD(master, salt []byte) (cipher.AEAD, error) {
	if len(master) != 32 || len(salt) != 16 {
		return nil, errors.New("backup key must be 32 bytes and archive salt 16 bytes")
	}
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("git-local backup archive key v1"))
	mac.Write(salt)
	derived := mac.Sum(nil)
	block, err := aes.NewCipher(derived)
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

func recordAAD(header []byte, size uint32) []byte {
	aad := make([]byte, len(header)+4)
	copy(aad, header)
	binary.BigEndian.PutUint32(aad[len(header):], size)
	return aad
}

func writeFull(dst io.Writer, data []byte) error {
	n, err := dst.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func writeRecord(dst io.Writer, aead cipher.AEAD, header []byte, counter, size uint32, plain []byte) error {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], size)
	if err := writeFull(dst, length[:]); err != nil {
		return err
	}
	sealed := aead.Seal(nil, nonce(counter), plain, recordAAD(header, size))
	return writeFull(dst, sealed)
}

// Encrypt writes independently authenticated 1 MiB records and an
// authenticated terminal record. It never writes plaintext to dst.
func Encrypt(dst io.Writer, src io.Reader, master []byte) error {
	if len(master) != 32 {
		return errors.New("backup key must be exactly 32 bytes")
	}
	header := make([]byte, len(archiveMagic)+16)
	copy(header, archiveMagic)
	if _, err := rand.Read(header[len(archiveMagic):]); err != nil {
		return err
	}
	aead, err := archiveAEAD(master, header[len(archiveMagic):])
	if err != nil {
		return err
	}
	if err := writeFull(dst, header); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	var counter uint32
	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if counter == ^uint32(0) {
				return errors.New("backup archive exceeds record limit")
			}
			if err := writeRecord(dst, aead, header, counter, uint32(n), buf[:n]); err != nil {
				return err
			}
			counter++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return writeRecord(dst, aead, header, counter, 0, nil)
		}
		if readErr != nil {
			return readErr
		}
	}
}

// Decrypt rejects modified, truncated and extended archives. Callers must
// stage its output until it returns successfully, because earlier records may
// have been written before a later authentication failure.
func Decrypt(dst io.Writer, src io.Reader, master []byte) error {
	if len(master) != 32 {
		return errors.New("backup key must be exactly 32 bytes")
	}
	header := make([]byte, len(archiveMagic)+16)
	if _, err := io.ReadFull(src, header); err != nil {
		return fmt.Errorf("truncated backup header: %w", err)
	}
	for i, b := range archiveMagic {
		if header[i] != b {
			return errors.New("unsupported backup archive format")
		}
	}
	aead, err := archiveAEAD(master, header[len(archiveMagic):])
	if err != nil {
		return err
	}
	var counter uint32
	for {
		var length [4]byte
		if _, err := io.ReadFull(src, length[:]); err != nil {
			return fmt.Errorf("truncated backup record: %w", err)
		}
		size := binary.BigEndian.Uint32(length[:])
		if size > chunkSize {
			return errors.New("invalid backup record size")
		}
		sealed := make([]byte, int(size)+aead.Overhead())
		if _, err := io.ReadFull(src, sealed); err != nil {
			return fmt.Errorf("truncated backup ciphertext: %w", err)
		}
		plain, err := aead.Open(nil, nonce(counter), sealed, recordAAD(header, size))
		if err != nil {
			return errors.New("backup authentication failed")
		}
		if size == 0 {
			if len(plain) != 0 {
				return errors.New("invalid backup terminal record")
			}
			var extra [1]byte
			if n, err := src.Read(extra[:]); n != 0 || err != io.EOF {
				return errors.New("trailing data after backup terminal record")
			}
			return nil
		}
		if err := writeFull(dst, plain); err != nil {
			return err
		}
		if counter == ^uint32(0) {
			return errors.New("backup archive exceeds record limit")
		}
		counter++
	}
}
