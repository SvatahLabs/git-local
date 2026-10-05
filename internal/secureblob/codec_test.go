package secureblob

import (
	"bytes"
	"crypto/rand"
	"io"
	"strconv"
	"strings"
	"testing"
)

func testIdentity() Identity {
	return Identity{RepositoryID: "repository", WorkspaceID: "workspace", Path: ".env.local", Mode: "100644"}
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestRoundTripEmptyAndMultipleRecords(t *testing.T) {
	key, id := testKey(t), testIdentity()
	for _, plain := range [][]byte{nil, []byte("TOKEN=private\n"), bytes.Repeat([]byte("\x00secret\xff"), ChunkSize/8+3)} {
		var encrypted bytes.Buffer
		if err := Encrypt(&encrypted, bytes.NewReader(plain), key, id); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encrypted.Bytes(), []byte("TOKEN=private")) {
			t.Fatal("plaintext appeared in encrypted blob")
		}
		var restored bytes.Buffer
		if err := Decrypt(&restored, bytes.NewReader(encrypted.Bytes()), key, id); err != nil || !bytes.Equal(restored.Bytes(), plain) {
			t.Fatalf("round trip: %v, %d bytes", err, restored.Len())
		}
		var encryptedAgain bytes.Buffer
		if err := Encrypt(&encryptedAgain, bytes.NewReader(plain), key, id); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(encrypted.Bytes(), encryptedAgain.Bytes()) {
			t.Fatal("encryption reused a random file salt")
		}
	}
}

func TestAuthenticationRejectsTamperingAndWrongIdentity(t *testing.T) {
	key, id := testKey(t), testIdentity()
	plain := bytes.Repeat([]byte("sensitive content\n"), ChunkSize/8+5)
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader(plain), key, id); err != nil {
		t.Fatal(err)
	}
	original := encrypted.Bytes()
	variants := map[string][]byte{}
	for _, index := range []int{len(magic), len(magic) + 16 + 4, len(original) - 1} {
		copyOf := bytes.Clone(original)
		copyOf[index] ^= 1
		variants["tampered-"+strconv.Itoa(index)] = copyOf
	}
	variants["truncated"] = bytes.Clone(original[:len(original)-1])
	variants["extended"] = append(bytes.Clone(original), 'x')
	for label, variant := range variants {
		t.Run(label, func(t *testing.T) {
			if err := Decrypt(io.Discard, bytes.NewReader(variant), key, id); err == nil {
				t.Fatal("modified ciphertext passed authentication")
			}
		})
	}
	for label, changed := range map[string]Identity{
		"repository": {"other", id.WorkspaceID, id.Path, id.Mode},
		"workspace":  {id.RepositoryID, "other", id.Path, id.Mode},
		"path":       {id.RepositoryID, id.WorkspaceID, "other", id.Mode},
		"mode":       {id.RepositoryID, id.WorkspaceID, id.Path, "100755"},
	} {
		t.Run(label, func(t *testing.T) {
			if err := Decrypt(io.Discard, bytes.NewReader(original), key, changed); err == nil {
				t.Fatal("ciphertext accepted for another file identity")
			}
		})
	}
	wrongKey := testKey(t)
	if err := Decrypt(io.Discard, bytes.NewReader(original), wrongKey, id); err == nil {
		t.Fatal("ciphertext accepted with wrong key")
	}
}

func TestRejectsInvalidInputsAndShortWrites(t *testing.T) {
	key, id := testKey(t), testIdentity()
	if err := Encrypt(io.Discard, strings.NewReader("x"), key[:31], id); err == nil {
		t.Fatal("short key accepted")
	}
	if err := Encrypt(io.Discard, strings.NewReader("x"), key, Identity{}); err == nil {
		t.Fatal("empty identity accepted")
	}
	if err := Encrypt(shortWriter{}, strings.NewReader("x"), key, id); err != io.ErrShortWrite {
		t.Fatalf("short write: %v", err)
	}
	if err := Decrypt(io.Discard, strings.NewReader("x"), key, id); err == nil {
		t.Fatal("truncated header accepted")
	}
}

func TestDigestBindsContentAndIdentity(t *testing.T) {
	key, id := testKey(t), testIdentity()
	first, err := Digest(strings.NewReader("TOKEN=abc"), key, id)
	if err != nil || len(first) != 64 {
		t.Fatalf("digest: %q %v", first, err)
	}
	second, err := Digest(strings.NewReader("TOKEN=abc"), key, id)
	if err != nil || second != first {
		t.Fatalf("unstable digest: %q %v", second, err)
	}
	id.Path = "other"
	otherPath, err := Digest(strings.NewReader("TOKEN=abc"), key, id)
	if err != nil || otherPath == first {
		t.Fatalf("path did not bind digest: %q %v", otherPath, err)
	}
	id = testIdentity()
	otherContent, err := Digest(strings.NewReader("TOKEN=def"), key, id)
	if err != nil || otherContent == first {
		t.Fatalf("content did not bind digest: %q %v", otherContent, err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) / 2, nil }
