package overlay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestAuditInstallationDetectsFilterAndAttributeDrift(t *testing.T) {
	ctx := context.Background()
	repo := testutil.New(t)
	path := filepath.Join(repo.Root, "app.conf")
	if err := os.WriteFile(path, []byte("port = 80\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), filepath.Base(source))
	program, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, program, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("port = 8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, r, repo.Root, "port", "app.conf", executable); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		err := AuditInstallation(ctx, r, repo.Root, w.Common)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("audit: got %v, want %q", err, want)
		}
	}
	check("")
	configPath := filepath.Join(w.Common, "gitlocal", filterConfigName)
	repo.Git("config", "--local", "--fixed-value", "--unset-all", "include.path", filterInclude)
	check("exactly one filter include")
	repo.Git("config", "--local", "--add", "include.path", configPath)
	check("absolute path")
	if _, err := Add(ctx, r, repo.Root, "port", "app.conf", executable); err != nil {
		t.Fatalf("add did not migrate the absolute include: %v", err)
	}
	check("")
	if got := strings.TrimSpace(string(repo.Git("config", "--local", "--get-all", "include.path"))); got != filterInclude {
		t.Fatalf("include after migration: %q", got)
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[filter \"gitlocal-overlay\"]\n\tprocess = cat\n\trequired = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check("configuration was changed")
	if _, err := Add(ctx, r, repo.Root, "port", "app.conf", executable); err == nil || !strings.Contains(err.Error(), "configuration was changed") {
		t.Fatalf("add accepted damaged filter config: %v", err)
	}
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	check("")
	repo.Git("config", "--local", "filter.gitlocal-overlay.required", "false")
	check("value other than true")
	repo.Git("config", "--local", "--unset", "filter.gitlocal-overlay.required")
	check("does not set")
	repo.Git("config", "--local", "filter.gitlocal-overlay.required", "true")
	check("")
	attributesPath := filepath.Join(w.Common, "info", "attributes")
	attributes, err := os.ReadFile(attributesPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attributesPath, append(append([]byte{}, attributes...), []byte("/app.conf -filter\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	check("no longer uses the required filter")
	if err := os.WriteFile(attributesPath, attributes, 0600); err != nil {
		t.Fatal(err)
	}
	check("")
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	check("executable is unavailable")
}
