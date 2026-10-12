package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name     string
	typeflag byte
	body     string
	link     string
}

func tarOf(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.typeflag == tar.TypeDir || e.typeflag == tar.TypeSymlink {
			size = 0
		}
		header := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0600, Size: size, Linkname: e.link}
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// GL-071: malformed or hostile archive entries are refused, and nothing is
// written outside the stage; a well-formed minimal archive is accepted.
func TestExtractTarRefusesMalformedEntries(t *testing.T) {
	identity := tarEntry{name: "identity.json", typeflag: tar.TypeReg, body: "{}"}
	state := tarEntry{name: "state/", typeflag: tar.TypeDir}
	for name, entries := range map[string][]tarEntry{
		"parent path":        {identity, state, {name: "state/../../escaped", typeflag: tar.TypeReg, body: "x"}},
		"absolute path":      {identity, state, {name: "/escaped", typeflag: tar.TypeReg, body: "x"}},
		"backslash":          {identity, state, {name: `state\escaped`, typeflag: tar.TypeReg, body: "x"}},
		"unexpected top":     {identity, state, {name: "escaped", typeflag: tar.TypeReg, body: "x"}},
		"symlink":            {identity, state, {name: "state/link", typeflag: tar.TypeSymlink, link: "../../escaped"}},
		"hard link":          {identity, state, {name: "state/link", typeflag: tar.TypeLink, link: "identity.json"}},
		"identity dir":       {{name: "identity.json/", typeflag: tar.TypeDir}, state},
		"state file":         {identity, {name: "state", typeflag: tar.TypeReg, body: "x"}},
		"oversized identity": {{name: "identity.json", typeflag: tar.TypeReg, body: strings.Repeat("x", 64<<10+1)}, state},
		"missing identity":   {state},
		"missing state":      {identity},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			stage := filepath.Join(parent, "stage")
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}
			if err := extractTar(context.Background(), stage, bytes.NewReader(tarOf(t, entries...))); err == nil {
				t.Fatal("malformed archive accepted")
			}
			if _, err := os.Lstat(filepath.Join(parent, "escaped")); !os.IsNotExist(err) {
				t.Fatalf("archive wrote outside its stage: %v", err)
			}
		})
	}
	t.Run("truncated", func(t *testing.T) {
		data := tarOf(t, identity, state, tarEntry{name: "state/file", typeflag: tar.TypeReg, body: strings.Repeat("x", 4096)})
		stage := t.TempDir()
		if err := extractTar(context.Background(), stage, bytes.NewReader(data[:len(data)-3000])); err == nil {
			t.Fatal("truncated archive accepted")
		}
	})
	stage := t.TempDir()
	if err := extractTar(context.Background(), stage, bytes.NewReader(tarOf(t, identity, state, tarEntry{name: "state/file", typeflag: tar.TypeReg, body: "kept"}))); err != nil {
		t.Fatalf("well-formed archive: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(stage, "state", "file")); err != nil || string(data) != "kept" {
		t.Fatalf("extracted file: %q %v", data, err)
	}
}
