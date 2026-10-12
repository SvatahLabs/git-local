// Package backup owns the encrypted archive format used for opt-in remote
// backups. A backup key is 32 random bytes supplied separately from the data.
package backup

import (
	"bytes"
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

// Version 2 archives bind their backup ref into the authenticated header.
// Version 1 archives are unbound and refused.
var (
	archiveMagic       = []byte("GITLOCAL-BACKUP\x02")
	legacyArchiveMagic = []byte("GITLOCAL-BACKUP\x01")
)

const maxBoundRef = 1024

// archiveHeader is the magic, the salt and the backup ref the archive is
// pushed under. Every record's authenticated data includes it, so an archive
// served under another ref (an older backup, or another repository's) fails.
func archiveHeader(salt []byte, ref string) ([]byte, error) {
	if ref == "" || len(ref) > maxBoundRef {
		return nil, errors.New("invalid backup ref for archive binding")
	}
	header := append(append([]byte{}, archiveMagic...), salt...)
	header = binary.BigEndian.AppendUint16(header, uint16(len(ref)))
	return append(header, ref...), nil
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
// authenticated terminal record, bound to ref. It never writes plaintext to dst.
func Encrypt(dst io.Writer, src io.Reader, master []byte, ref string) error {
	if len(master) != 32 {
		return errors.New("backup key must be exactly 32 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	header, err := archiveHeader(salt, ref)
	if err != nil {
		return err
	}
	aead, err := archiveAEAD(master, salt)
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

// Decrypt rejects modified, truncated and extended archives, and archives
// bound to a ref other than ref. Callers must stage its output until it
// returns successfully, because earlier records may have been written before
// a later authentication failure.
func Decrypt(dst io.Writer, src io.Reader, master []byte, ref string) error {
	if len(master) != 32 {
		return errors.New("backup key must be exactly 32 bytes")
	}
	prefix := make([]byte, len(archiveMagic)+16+2)
	if _, err := io.ReadFull(src, prefix); err != nil {
		return fmt.Errorf("truncated backup header: %w", err)
	}
	if bytes.Equal(prefix[:len(legacyArchiveMagic)], legacyArchiveMagic) {
		return errors.New("backup archive was written by an earlier release and isn't bound to its backup ref, so its age and repository can't be verified; restore it with that release, then push a new backup")
	}
	if !bytes.Equal(prefix[:len(archiveMagic)], archiveMagic) {
		return errors.New("unsupported backup archive format")
	}
	salt := prefix[len(archiveMagic) : len(archiveMagic)+16]
	size := int(binary.BigEndian.Uint16(prefix[len(archiveMagic)+16:]))
	if size == 0 || size > maxBoundRef {
		return errors.New("invalid backup archive binding")
	}
	bound := make([]byte, size)
	if _, err := io.ReadFull(src, bound); err != nil {
		return fmt.Errorf("truncated backup header: %w", err)
	}
	if string(bound) != ref {
		return fmt.Errorf("backup archive names ref %q (unverified), not %s; the backup remote may have swapped refs", bound, ref)
	}
	header, err := archiveHeader(salt, ref)
	if err != nil {
		return err
	}
	aead, err := archiveAEAD(master, salt)
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
