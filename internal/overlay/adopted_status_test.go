package overlay

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitlocal/internal/testutil"
)

// TestOverlayStatusNamesHunksAlreadyUpstream: a teammate commits one of the
// overlay's two hunks upstream. overlay status and doctor must say the
// overlay is stale and how to save it again.
func TestOverlayStatusNamesHunksAlreadyUpstream(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\npassword = none\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	repo.Git("push", "-q", "-u", "origin", "main")
	bin := integrationBinary(t)
	local := func(args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		return cmd.CombinedOutput()
	}
	if out, err := local("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = s3cret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := local("overlay", "add", "dev", "--", "app.conf"); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	}
	if out, err := local("overlay", "status"); err != nil || bytes.Contains(out, []byte("stale")) {
		t.Fatalf("status before adoption: %v\n%s", err, out)
	}
	teammate := filepath.Join(t.TempDir(), "teammate")
	repo.Git("clone", "-q", "-b", "main", remote, teammate)
	if err := os.WriteFile(filepath.Join(teammate, "app.conf"), []byte("port = 8080\na\nb\nc\nd\ne\nf\npassword = none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.At(teammate, "commit", "-qam", "use port 8080")
	repo.At(teammate, "push", "-q", "origin", "main")
	repo.Git("fetch", "-q", "origin")
	out, err := local("overlay", "status")
	if err != nil || !bytes.Contains(out, []byte("1 of 2 hunks are already upstream")) || !bytes.Contains(out, []byte("git local overlay remove dev")) || !bytes.Contains(out, []byte("git local overlay add dev -- 'app.conf'")) {
		t.Fatalf("status after adoption: %v\n%s", err, out)
	}
	out, err = local("overlay", "status", "--json")
	var envelope struct {
		Data struct {
			Overlays []struct {
				AdoptedHunks int `json:"adoptedHunks"`
				Hunks        int `json:"hunks"`
			} `json:"overlays"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal(out, &envelope) != nil || len(envelope.Data.Overlays) != 1 || envelope.Data.Overlays[0].AdoptedHunks != 1 || envelope.Data.Overlays[0].Hunks != 2 {
		t.Fatalf("status JSON: %v\n%s", err, out)
	}
	if out, _ := local("doctor"); !bytes.Contains(out, []byte("overlay dev on app.conf is stale")) || !bytes.Contains(out, []byte("git local overlay remove dev")) || !bytes.Contains(out, []byte("git local overlay add dev -- 'app.conf'")) {
		t.Fatalf("doctor:\n%s", out)
	}
}
