package history

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/storage"
)

func TestBlobBatchLeavesOversizedFileForStreaming(t *testing.T) {
	root := t.TempDir()
	file, err := os.OpenFile(filepath.Join(root, "large.bin"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(blobBatchBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := storage.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	batch, err := newBlobBatch()
	if err != nil {
		t.Fatal(err)
	}
	defer batch.close()
	if _, err := batch.stage(files, "large.bin", "100644", 0600); !errors.Is(err, errBlobBatchTooLarge) {
		t.Fatalf("large blob was staged in the batch: %v", err)
	}
	if len(batch.items) != 0 || batch.bytes != 0 {
		t.Fatal("oversized blob changed batch state")
	}
}
