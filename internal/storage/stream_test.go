package storage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamStableRejectsMutationAndShortConsumption(t *testing.T) {
	d, root := testDir(t)
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.StreamStable("file", func(r io.Reader) error { return nil }); err == nil {
		t.Fatal("accepted unread bytes")
	}
	if _, err := d.StreamStable("file", func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "file"), []byte("longer"), 0600)
	}); err == nil {
		t.Fatal("accepted mutation during read")
	}
	var got bytes.Buffer
	observation, err := d.StreamStable("file", func(r io.Reader) error { _, e := io.Copy(&got, r); return e })
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "longer" {
		t.Fatal("stream changed bytes")
	}
	if err = os.WriteFile(filepath.Join(root, "file"), []byte("even longer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = observation.Recheck(d, "file"); err == nil {
		t.Fatal("recheck missed edit")
	}
}
