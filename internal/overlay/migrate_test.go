package overlay

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

func TestInitMigratesEarlierOverlayInstallWithoutNewVersion(t *testing.T) {
	repo := testutil.New(t)
	repo.Git("config", "core.autocrlf", "false")
	base := []byte("port = 80\na\nb\nc\n")
	patch := []byte("port = 8080\na\nb\nc\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("git-local %v: %v\n%s", args, err, out)
		}
		return out
	}
	must("init")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	must("overlay", "add", "port", "--", "app.conf")
	gitlocal := func() string { return filepath.Join(repo.Root, ".git", "gitlocal") }
	version := func() int {
		t.Helper()
		matches, err := filepath.Glob(filepath.Join(gitlocal(), "overlays", "*", "port", "manifest.json"))
		if err != nil || len(matches) != 1 {
			t.Fatal(matches, err)
		}
		data, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m.Version
	}
	before := version()
	// What 0.1.4 left: an absolute include and no direct required key.
	legacy := func() {
		t.Helper()
		repo.Git("config", "--local", "--replace-all", "include.path", filepath.Join(gitlocal(), filterConfigName))
		repo.Git("config", "--local", "--unset-all", requiredKey)
	}
	includes := func() string {
		return strings.TrimSpace(string(repo.Git("config", "--local", "--get-all", "include.path")))
	}
	staysClean := func() {
		t.Helper()
		edited := bytes.Replace(patch, []byte("c\n"), []byte("c2\n"), 1)
		if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), edited, 0600); err != nil {
			t.Fatal(err)
		}
		repo.Git("add", "app.conf")
		if staged := repo.Git("cat-file", "blob", ":app.conf"); !bytes.Equal(staged, bytes.Replace(base, []byte("c\n"), []byte("c2\n"), 1)) {
			t.Fatalf("staged content: %q", staged)
		}
		repo.Git("reset", "-q", "app.conf")
		if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), patch, 0600); err != nil {
			t.Fatal(err)
		}
	}
	legacy()
	if out := must("doctor"); !strings.Contains(out, "WARNING overlays") || !strings.Contains(out, "git local init") {
		t.Fatalf("doctor on an unmigrated install:\n%s", out)
	}
	if out, err := run("reset", "--hard", "HEAD"); err != nil {
		t.Fatalf("reset --hard on an unmoved, unmigrated install: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(repo.Root, "app.conf")); !bytes.Equal(got, patch) {
		t.Fatalf("reset lost the overlay: %q", got)
	}
	out := must("init")
	if !strings.Contains(out, "rewrote the absolute filter include") || !strings.Contains(out, requiredKey) {
		t.Fatalf("init output:\n%s", out)
	}
	if includes() != filterInclude || strings.TrimSpace(string(repo.Git("config", "--local", "--get", requiredKey))) != "true" || version() != before {
		t.Fatalf("after init: include %q version %d (was %d)", includes(), version(), before)
	}
	if out := must("doctor"); strings.Contains(out, "overlays:") && !strings.Contains(out, "OK overlays") {
		t.Fatalf("doctor after migration:\n%s", out)
	}
	if out := must("init"); strings.Contains(out, "Overlay filter:") {
		t.Fatalf("second init changed something:\n%s", out)
	}
	staysClean()

	// A deleted include entry and a deleted filter config are restored.
	repo.Git("config", "--local", "--unset-all", "include.path")
	if err := os.Remove(filepath.Join(gitlocal(), filterConfigName)); err != nil {
		t.Fatal(err)
	}
	out = must("init")
	if !strings.Contains(out, "restored the filter include") || !strings.Contains(out, "recreated the filter configuration") || includes() != filterInclude || version() != before {
		t.Fatalf("restore: %q\n%s", includes(), out)
	}
	staysClean()

	// After the checkout moves, an unmigrated install is refused by reset and
	// migrated by init.
	legacy()
	moved := filepath.Join(filepath.Dir(repo.Root), "moved-checkout")
	if err := os.Rename(repo.Root, moved); err != nil {
		t.Fatal(err)
	}
	repo.Root = moved
	if out, err := run("reset", "--hard", "HEAD"); err == nil || !strings.Contains(out, "no longer resolves") {
		t.Fatalf("reset accepted a moved, unmigrated install: %v\n%s", err, out)
	}
	if out := must("init"); !strings.Contains(out, "rewrote the absolute filter include") {
		t.Fatalf("init from the moved checkout:\n%s", out)
	}
	must("reset", "--hard", "HEAD")
	staysClean()

	// An include of another repository's filter config is never ours.
	other := testutil.New(t)
	foreign := filepath.Join(other.Root, ".git", "gitlocal", filterConfigName)
	if err := os.MkdirAll(filepath.Dir(foreign), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("[filter \"gitlocal-overlay\"]\n\tprocess = cat\n\trequired = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("config", "--local", "--replace-all", "include.path", foreign)
	for _, args := range [][]string{{"init"}, {"reset", "--hard", "HEAD"}, {"doctor"}} {
		if out, err := run(args...); err == nil || !strings.Contains(out, "another repository") {
			t.Fatalf("%v accepted another repository's filter include: %v\n%s", args, err, out)
		}
	}
	if includes() != foreign {
		t.Fatalf("init rewrote a foreign include: %q", includes())
	}
}
