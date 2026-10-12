package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"testing"
)

// GenerateKey creates a raw AES-256 master key for tests; the CLI writes keys
// with keymaterial.Create.
func GenerateKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	return key, err
}

func TestEncryptedArchiveRoundTripAndRandomization(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, 2*chunkSize+17)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := Encrypt(&first, bytes.NewReader(plain), key, codecRef); err != nil {
		t.Fatal(err)
	}
	if err := Encrypt(&second, bytes.NewReader(plain), key, codecRef); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Bytes(), second.Bytes()) || bytes.Contains(first.Bytes(), plain[:64]) {
		t.Fatal("backup encryption reused output or exposed plaintext")
	}
	var recovered bytes.Buffer
	if err := Decrypt(&recovered, bytes.NewReader(first.Bytes()), key, codecRef); err != nil || !bytes.Equal(recovered.Bytes(), plain) {
		t.Fatal("backup round trip failed", err)
	}
	wrong := bytes.Clone(key)
	wrong[0] ^= 1
	if err := Decrypt(io.Discard, bytes.NewReader(first.Bytes()), wrong, codecRef); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestEncryptedArchiveRejectsTamperingTruncationAndExtension(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte("local history secret\n"), 70000)
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader(plain), key, codecRef); err != nil {
		t.Fatal(err)
	}
	data := encrypted.Bytes()
	for _, at := range []int{0, len(archiveMagic), len(archiveMagic) + 16, len(data) / 2, len(data) - 1} {
		modified := bytes.Clone(data)
		modified[at] ^= 1
		if err := Decrypt(io.Discard, bytes.NewReader(modified), key, codecRef); err == nil {
			t.Fatalf("modified byte %d accepted", at)
		}
	}
	for _, size := range []int{0, len(archiveMagic) + 10, len(data) / 2, len(data) - 1} {
		if err := Decrypt(io.Discard, bytes.NewReader(data[:size]), key, codecRef); err == nil {
			t.Fatalf("truncated archive of %d bytes accepted", size)
		}
	}
	tooLong := append(bytes.Clone(data), 1)
	if err := Decrypt(io.Discard, bytes.NewReader(tooLong), key, codecRef); err == nil {
		t.Fatal("trailing byte accepted")
	}
	invalidLength := bytes.Clone(data)
	binary.BigEndian.PutUint32(invalidLength[len(archiveMagic)+16:], chunkSize+1)
	if err := Decrypt(io.Discard, bytes.NewReader(invalidLength), key, codecRef); err == nil {
		t.Fatal("oversized encrypted record accepted")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestEncryptedArchiveRejectsInvalidKeysAndShortWrites(t *testing.T) {
	if err := Encrypt(io.Discard, bytes.NewReader(nil), []byte("short"), codecRef); err == nil {
		t.Fatal("short key accepted for encryption")
	}
	if err := Decrypt(io.Discard, bytes.NewReader(nil), []byte("short"), codecRef); err == nil {
		t.Fatal("short key accepted for decryption")
	}
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := Encrypt(shortWriter{}, bytes.NewReader([]byte("a")), key, codecRef); err != io.ErrShortWrite {
		t.Fatal("short archive write accepted", err)
	}
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader([]byte("a")), key, codecRef); err != nil {
		t.Fatal(err)
	}
	if err := Decrypt(shortWriter{}, bytes.NewReader(encrypted.Bytes()), key, codecRef); err != io.ErrShortWrite {
		t.Fatal("short decrypted write accepted", err)
	}
}

const codecRef = "refs/gitlocal/backups/00000000-0000-4000-8000-000000000000/20260101T000000Z-00000000-0000-4000-8000-000000000001"

// testRef names a backup of repositoryID.
func testRef(repositoryID string) string {
	return "refs/gitlocal/backups/" + repositoryID + "/20260101T000000Z-00000000-0000-4000-8000-000000000001"
}

func TestDecryptRefusesOtherRefAndUnboundArchive(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader([]byte("state")), key, codecRef); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := Decrypt(&plain, bytes.NewReader(encrypted.Bytes()), key, codecRef); err != nil || plain.String() != "state" {
		t.Fatalf("bound archive: %q %v", plain.String(), err)
	}
	other := testRef("00000000-0000-4000-8000-000000000002")
	if err := Decrypt(io.Discard, bytes.NewReader(encrypted.Bytes()), key, other); err == nil || !strings.Contains(err.Error(), strconv.Quote(codecRef)+" (unverified)") {
		t.Fatalf("archive accepted under another ref: %v", err)
	}
	// Rewriting the bound ref in the header (same length) fails authentication.
	forged := bytes.Replace(encrypted.Bytes(), []byte(codecRef), []byte(other[:len(codecRef)]), 1)
	if err := Decrypt(io.Discard, bytes.NewReader(forged), key, other[:len(codecRef)]); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("forged binding: %v", err)
	}
	legacy := append([]byte("GITLOCAL-BACKUP\x01"), make([]byte, 64)...)
	if err := Decrypt(io.Discard, bytes.NewReader(legacy), key, codecRef); err == nil || !strings.Contains(err.Error(), "earlier release") {
		t.Fatalf("unbound archive: %v", err)
	}
}
