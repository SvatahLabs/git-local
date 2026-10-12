package teardown

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/hook"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// GL-112: deinit sets the private state aside under the exclusive lock, after
// a final check, so hooks another command installed after deinit removed its
// own are found: deinit refuses and keeps the state, and a rerun finishes.
// Not parallel: it replaces beforeSetAside.
func TestDeinitRefusesWhatWasInstalledMeanwhile(t *testing.T) {
	ctx := context.Background()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	beforeSetAside = func() {
		if _, _, err := hook.Install(ctx, r, repo.Root, executable, true, true, false); err != nil {
			t.Errorf("install hooks during deinit: %v", err)
		}
	}
	t.Cleanup(func() { beforeSetAside = func() {} })
	if _, err := Deinit(ctx, r, repo.Root, false); err == nil || !strings.Contains(err.Error(), "hooks") {
		t.Fatalf("deinit set the private state aside while hooks were installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, ".git", "gitlocal")); err != nil {
		t.Fatalf("refused deinit set the private state aside: %v", err)
	}
	beforeSetAside = func() {}
	result, err := Deinit(ctx, r, repo.Root, false)
	if err != nil || result.Archive == "" || len(result.Hooks) == 0 {
		t.Fatalf("rerun: %+v %v", result, err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo.Root, ".git", "hooks", "pre-commit")); bytes.Contains(data, []byte("git-local")) {
		t.Fatalf("hook left after the rerun:\n%s", data)
	}
}
