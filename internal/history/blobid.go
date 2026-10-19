package history

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/SvatahLabs/git-local/internal/storage"
)

// blobID computes the ID `git hash-object --no-filters` gives size bytes from
// input, without starting Git. Private stores are always SHA-1 (gitexec's
// StoreConfig admits no objectformat extension).
func blobID(size int64, input io.Reader) (string, error) {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	n, err := io.Copy(h, input)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("%w: file size changed while hashing", storage.ErrConflict)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// blobIDString is blobID for content held in memory, such as a link target.
func blobIDString(content string) string {
	id, _ := blobID(int64(len(content)), strings.NewReader(content))
	return id
}

// treeID computes the ID Git gives a tree of entries: sorted by name, a
// tree's name compared as if it ended in a slash.
func treeID(entries []treeEntry) (string, error) {
	sorted := append([]treeEntry{}, entries...)
	key := func(e treeEntry) string {
		if e.kind == "tree" {
			return e.name + "/"
		}
		return e.name
	}
	sort.Slice(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	var body bytes.Buffer
	for i, e := range sorted {
		if i > 0 && sorted[i-1].name == e.name {
			return "", fmt.Errorf("duplicate tree entry %q", e.name)
		}
		raw, err := hex.DecodeString(e.id)
		if err != nil || len(raw) != sha1.Size {
			return "", fmt.Errorf("invalid object ID %q", e.id)
		}
		body.WriteString(strings.TrimPrefix(e.mode, "0") + " " + e.name + "\x00")
		body.Write(raw)
	}
	h := sha1.New()
	h.Write([]byte("tree " + strconv.Itoa(body.Len()) + "\x00"))
	h.Write(body.Bytes())
	return hex.EncodeToString(h.Sum(nil)), nil
}
