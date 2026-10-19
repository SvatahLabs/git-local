package gitexec

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// One cat-file process streams many blobs, byte for byte; a missing object
// is an error naming it, and a failed copy of a blob larger than the pipe
// still lets Close end the process.
func TestBlobReaderStreamsManyBlobsThroughOneProcess(t *testing.T) {
	repo := testutil.New(t)
	r := newRunner(t, repo)
	ctx := context.Background()
	store := filepath.Join(t.TempDir(), "history.git")
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	contents := []string{"", "one\n", "two\x00\xff", strings.Repeat("large\n", 200000)}
	var ids []string
	for _, c := range contents {
		id, err := r.History(ctx, store, strings.NewReader(c), "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, strings.TrimSpace(string(id)))
	}
	before := r.SubprocessCount()
	blobs, err := r.HistoryBlobs(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		for i, id := range ids {
			var out bytes.Buffer
			if err := blobs.Blob(id, &out); err != nil || out.String() != contents[i] {
				t.Fatalf("blob %d: %d bytes, %v", i, out.Len(), err)
			}
		}
	}
	if err := blobs.Close(); err != nil {
		t.Fatal(err)
	}
	if n := r.SubprocessCount() - before; n != 1 {
		t.Fatalf("12 blobs ran %d Git processes", n)
	}

	blobs, err = r.HistoryBlobs(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("0", len(ids[0]))
	if err := blobs.Blob(missing, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing blob: %v", err)
	}
	blobs.Close()

	blobs, err = r.HistoryBlobs(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	// A writer that takes every byte but fails (a fault point after a write)
	// stops the reader too.
	failure := errors.New("destination full")
	if err := blobs.Blob(ids[1], acceptingFailingWriter{failure}); !errors.Is(err, failure) {
		t.Fatalf("write error after the last byte: %v", err)
	}
	blobs.Close()
	if blobs, err = r.HistoryBlobs(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := blobs.Blob(ids[3], failingWriter{failure}); !errors.Is(err, failure) {
		t.Fatalf("failed copy: %v", err)
	}
	if err := blobs.Blob(ids[1], &bytes.Buffer{}); !errors.Is(err, failure) {
		t.Fatalf("reader used after a failed copy: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- blobs.Close() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Close hung after a failed copy")
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

type acceptingFailingWriter struct{ err error }

func (w acceptingFailingWriter) Write(p []byte) (int, error) { return len(p), w.err }
