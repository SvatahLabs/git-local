package overlay

import (
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/storage"
)

func TestBackupArchivesAllOverlayVersionsAndRejectsCorruption(t *testing.T) {
	state, err := storage.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishVersionLocked(state, id, "port", "app.conf", []byte("port=80\n"), []byte("port=8080\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := publishVersionLocked(state, id, "port", "app.conf", []byte("port=80\n"), []byte("port=9090\n")); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveForAdoption(state); err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := state.ReadJSON(filepath.Join(prefix(id, "port"), "manifest.json"), &manifest); err != nil || manifest.State != "removed" || manifest.Version != 2 {
		t.Fatalf("archived manifest: %+v %v", manifest, err)
	}
	for version := uint32(1); version <= 2; version++ {
		if _, err := readVersion(state, manifest, version); err != nil {
			t.Fatalf("lost saved version %d: %v", version, err)
		}
	}
	if err := state.AtomicWrite(filepath.Join(versionPath(id, "port", 1), "local"), []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBackupRecords(state); err == nil {
		t.Fatal("accepted corrupt older overlay version")
	}
}
