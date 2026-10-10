package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestAbsoluteOnAnyOS(t *testing.T) {
	for path, want := range map[string]bool{
		"/home/a/repo":              true,
		"/Users/a/repo/.git":        true,
		`C:\Users\a\repo`:           true,
		`c:/Users/a/repo/.git`:      true,
		`\\server\share\repo`:       true,
		"relative/repo":             false,
		`C:relative`:                false,
		"":                          false,
		"/repo\x00":                 false,
		`\single\backslash`:         false,
		"~/repo":                    false,
		`1:\not-a-drive`:            false,
		"C:":                        false,
		`\\`:                        false,
		"//server/share/repo":       true,
		"/":                         true,
		`D:\`:                       true,
		"/tmp/windows\\style/mixed": true,
	} {
		if got := absoluteOnAnyOS(path); got != want {
			t.Errorf("absoluteOnAnyOS(%q) = %v, want %v", path, got, want)
		}
	}
}

// A backup from another OS records the source machine's workspace paths. They
// are metadata: restore accepts them, rewrites the restored workspace's own
// paths, and keeps the other workspace retained with its recorded paths.
func TestRestoreAcceptsWorkspacePathsFromAnotherOS(t *testing.T) {
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
	linked := filepath.Join(t.TempDir(), "linked")
	source.Git("worktree", "add", "-q", "-b", "other", linked)
	other, err := workspace.Init(ctx, g, linked)
	if err != nil {
		t.Fatal(err)
	}
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	var ref, sourceID string
	if err := configtx.ReadSelection(ctx, g, source.Root, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		ref, sourceID = testRef(w.RepositoryID), w.WorkspaceID
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
	foreign := map[string][2]string{
		sourceID:          {`C:\Users\a\repo`, `C:\Users\a\repo\.git`},
		other.WorkspaceID: {`D:\work\linked`, `C:\Users\a\repo\.git\worktrees\linked`},
	}
	for id, paths := range foreign {
		name := filepath.Join(stage, "state", "workspaces", id, "workspace.json")
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		record["rootPath"], record["adminPath"] = paths[0], paths[1]
		if data, err = json.Marshal(record); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, clone)
	cmd.Env = source.Env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	info, err := AdoptStaged(ctx, g, clone, stage)
	if err != nil || info.WorkspaceID != sourceID {
		t.Fatalf("restore a backup with Windows workspace paths: %+v %v", info, err)
	}
	var restored, retained workspace.Record
	for id, record := range map[string]*workspace.Record{sourceID: &restored, other.WorkspaceID: &retained} {
		data, err := os.ReadFile(filepath.Join(clone, ".git", "gitlocal", "workspaces", id, "workspace.json"))
		if err != nil || json.Unmarshal(data, record) != nil {
			t.Fatalf("restored record %s: %v", id, err)
		}
	}
	if restored.Root != clone && filepath.Clean(restored.Root) != mustEval(t, clone) || retained.State != "retained" || retained.Root != `D:\work\linked` {
		t.Fatalf("restored records: %+v %+v", restored, retained)
	}
	if data, err := history.Show(ctx, g, clone, sourceID, "latest", "secret.local"); err != nil || string(data) != "private\n" {
		t.Fatalf("restored history: %q %v", data, err)
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
