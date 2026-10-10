package backup

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestAdoptNamesStagingFileOwnedByMainline(t *testing.T) {
	source := testutil.New(t)
	source.Commit()
	g, err := gitexec.New(source.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx := context.Background()
	if _, err := workspace.Init(ctx, g, source.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Root, "secret.local"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, g, source.Root, true, "secret.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, g, source.Root, "snapshot"); err != nil {
		t.Fatal(err)
	}
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	var ref string
	if err := configtx.ReadSelection(ctx, g, source.Root, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		ref = testRef(w.RepositoryID)
		if err := PackEncrypted(ctx, filepath.Join(w.Common, "gitlocal"), w.Admin, &encrypted, key, ref); err != nil {
			return err
		}
		return recheck()
	}); err != nil {
		t.Fatal(err)
	}
	stage, err := DecryptStaged(ctx, bytes.NewReader(encrypted.Bytes()), key, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, clone)
	cmd.Env = source.Env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, output)
	}
	staging := ".gitlocal-tmp-0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69"
	if err := os.WriteFile(filepath.Join(clone, staging), []byte("decrypted"), 0600); err != nil {
		t.Fatal(err)
	}
	source.At(clone, "add", "-f", staging)
	if _, err := AdoptStaged(ctx, g, clone, stage); err == nil || !strings.Contains(err.Error(), "staging file") || !strings.Contains(err.Error(), staging) || strings.Contains(err.Error(), "selected path") {
		t.Fatalf("adoption message: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "gitlocal")); !os.IsNotExist(err) {
		t.Fatalf("refused adoption changed the checkout: %v", err)
	}
}
