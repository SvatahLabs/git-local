package overlay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func integrationBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("GITLOCAL_TEST_BINARY"); path != "" {
		info, err := os.Stat(path)
		if err != nil || !filepath.IsAbs(path) || !info.Mode().IsRegular() {
			t.Fatalf("invalid prebuilt git-local test binary %q: %v", path, err)
		}
		return path
	}
	name := "git-local"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, "../../cmd/git-local")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build filter binary: %v\n%s", err, out)
	}
	return bin
}

func TestRealGitFilterStripsAndReappliesOverlay(t *testing.T) {
	repo := testutil.New(t)
	repo.Git("config", "core.autocrlf", "false")
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(context.Background(), r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	local := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	bin := integrationBinary(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), local, 0600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command(bin, "--json", "overlay", "add", "port", "--", "app.conf")
	add.Dir, add.Env = repo.Root, repo.RunnerEnv()
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	} else {
		var result struct {
			SchemaVersion int  `json:"schemaVersion"`
			OK            bool `json:"ok"`
			Data          struct {
				Name    string `json:"name"`
				Path    string `json:"path"`
				Version int    `json:"version"`
				State   string `json:"state"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &result); err != nil || result.SchemaVersion != 1 || !result.OK || result.Data.Name != "port" || result.Data.Path != "app.conf" || result.Data.Version != 1 || result.Data.State != "active" {
			t.Fatalf("overlay add JSON: %v\n%s", err, out)
		}
	}
	repo.Git("add", "app.conf")
	if staged := repo.Git("cat-file", "blob", ":app.conf"); !bytes.Equal(staged, base) {
		t.Fatalf("overlay entered index: %q", staged)
	}
	working := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = debug\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), working, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	clean := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = debug\n")
	if staged := repo.Git("cat-file", "blob", ":app.conf"); !bytes.Equal(staged, clean) {
		t.Fatalf("ordinary edit or overlay staged incorrectly: %q", staged)
	}
	repo.Git("commit", "-qm", "ordinary edit")
	if committed := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(committed, clean) {
		t.Fatalf("overlay entered commit: %q", committed)
	}
	repo.Git("checkout", "-q", "--", "app.conf")
	if after, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(after, working) {
		t.Fatalf("checkout did not reapply overlay: %v %q", err, after)
	}
	repo.Git("checkout", "-q", "-b", "upstream")
	upstreamWorking := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = warn\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), upstreamWorking, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "upstream log change")
	repo.Git("checkout", "-q", "main")
	if after, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(after, working) {
		t.Fatalf("switch to main lost overlay: %v %q", err, after)
	}
	repo.Git("checkout", "-q", "upstream")
	if after, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(after, upstreamWorking) {
		t.Fatalf("switch to upstream lost overlay: %v %q", err, after)
	}
	updatedWorking := []byte("port = 8181\na\nb\nc\nd\ne\nf\ng\nlog = warn\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), updatedWorking, 0600); err != nil {
		t.Fatal(err)
	}
	update := exec.Command(bin, "overlay", "add", "port", "--", "app.conf")
	update.Dir, update.Env = repo.Root, repo.RunnerEnv()
	if out, err := update.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("version 2")) {
		t.Fatalf("overlay version update: %v\n%s", err, out)
	}
	repo.Git("add", "app.conf")
	if staged := repo.Git("cat-file", "blob", ":app.conf"); !bytes.Equal(staged, []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = warn\n")) {
		t.Fatalf("updated overlay entered index: %q", staged)
	}
	commitAWorking := []byte("port = 8181\na\nb\nc\nd\ne\nf\ng\nlog = error\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), commitAWorking, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("commit", "-qam", "commit -a ordinary edit")
	if committed := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(committed, []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = error\n")) {
		t.Fatalf("commit -a leaked overlay: %q", committed)
	}
	altIndex := filepath.Join(t.TempDir(), "index")
	indexBytes, err := os.ReadFile(filepath.Join(w.Common, "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(altIndex, indexBytes, 0600); err != nil {
		t.Fatal(err)
	}
	altWorking := []byte("port = 8181\na\nb\nc\nd\ne\nf\ng\nlog = trace\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), altWorking, 0600); err != nil {
		t.Fatal(err)
	}
	altAdd := exec.Command(repo.GitPath, "add", "app.conf")
	altAdd.Dir, altAdd.Env = repo.Root, append(append([]string{}, repo.Env...), "GIT_INDEX_FILE="+altIndex)
	if out, err := altAdd.CombinedOutput(); err != nil {
		t.Fatalf("alternate-index add: %v\n%s", err, out)
	}
	altShow := exec.Command(repo.GitPath, "cat-file", "blob", ":app.conf")
	altShow.Dir, altShow.Env = repo.Root, append(append([]string{}, repo.Env...), "GIT_INDEX_FILE="+altIndex)
	if staged, err := altShow.CombinedOutput(); err != nil || !bytes.Equal(staged, []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = trace\n")) {
		t.Fatalf("alternate index leaked overlay: %v %q", err, staged)
	}
	partial := exec.Command(repo.GitPath, "add", "-p", "--", "app.conf")
	partial.Dir, partial.Env = repo.Root, repo.Env
	partial.Stdin = bytes.NewBufferString("y\n")
	if out, err := partial.CombinedOutput(); err != nil {
		t.Fatalf("partial staging: %v\n%s", err, out)
	}
	if staged := repo.Git("cat-file", "blob", ":app.conf"); !bytes.Equal(staged, []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = trace\n")) {
		t.Fatalf("partial staging leaked overlay or lost ordinary edit: %q", staged)
	}
}

func TestResetWrapperPreservesManagedOverlay(t *testing.T) {
	repo := testutil.New(t)
	repo.Git("config", "core.autocrlf", "false")
	base := []byte("port = 80\na\nb\nc\n")
	local := []byte("port = 8080\na\nb\nc\n")
	path := filepath.Join(repo.Root, "app.conf")
	if err := os.WriteFile(path, base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := workspace.Init(context.Background(), r, repo.Root); err != nil {
		t.Fatal(err)
	}
	bin := integrationBinary(t)
	if err := os.WriteFile(path, local, 0600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command(bin, "overlay", "add", "port", "--", "app.conf")
	add.Dir, add.Env = repo.Root, repo.RunnerEnv()
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	}
	reset := exec.Command(bin, "reset", "--hard")
	reset.Dir, reset.Env = repo.Root, repo.RunnerEnv()
	if out, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("reset with managed overlay: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, local) {
		t.Fatalf("reset lost local overlay: %v %q", err, got)
	}
	if committed := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(committed, base) {
		t.Fatalf("overlay entered mainline commit: %q", committed)
	}
}

func TestOverlayFastForwardPullAndConflictingUpstream(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := workspace.Init(context.Background(), r, repo.Root); err != nil {
		t.Fatal(err)
	}
	bin := integrationBinary(t)
	local := bytes.Replace(base, []byte("port = 80"), []byte("port = 8080"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), local, 0600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command(bin, "overlay", "add", "port", "--", "app.conf")
	add.Dir, add.Env = repo.Root, repo.RunnerEnv()
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	}
	repo.Git("add", "app.conf")
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	repo.Git("push", "-q", "-u", "origin", "main")
	other := filepath.Join(t.TempDir(), "other")
	repo.Git("clone", "-q", "-b", "main", remote, other)
	otherGit := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(repo.GitPath, args...)
		cmd.Dir, cmd.Env = other, repo.Env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("other git %v: %v\n%s", args, err, out)
		}
		return out
	}
	upstream := bytes.Replace(base, []byte("log = info"), []byte("log = warn"), 1)
	if err := os.WriteFile(filepath.Join(other, "app.conf"), upstream, 0600); err != nil {
		t.Fatal(err)
	}
	otherGit("add", "app.conf")
	otherGit("commit", "-qm", "upstream log")
	otherGit("push", "-q", "origin", "main")
	repo.Git("pull", "-q", "--ff-only", "origin", "main")
	want := bytes.Replace(upstream, []byte("port = 80"), []byte("port = 8080"), 1)
	if got, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("pull did not reapply overlay: %v %q", err, got)
	}
	if staged := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(staged, upstream) {
		t.Fatalf("pull changed mainline blob: %q", staged)
	}
	conflicting := bytes.Replace(upstream, []byte("port = 80"), []byte("port = 9090"), 1)
	if err := os.WriteFile(filepath.Join(other, "app.conf"), conflicting, 0600); err != nil {
		t.Fatal(err)
	}
	otherGit("add", "app.conf")
	otherGit("commit", "-qm", "conflicting port")
	otherGit("push", "-q", "origin", "main")
	pull := exec.Command(repo.GitPath, "pull", "-q", "--ff-only", "origin", "main")
	pull.Dir, pull.Env = repo.Root, repo.Env
	if out, err := pull.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("overlay conflicts")) {
		t.Fatalf("conflicting pull should fail through required filter: %v\n%s", err, out)
	}
	if got := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(got, upstream) {
		t.Fatalf("conflicting pull moved HEAD: %q", got)
	}
	status := exec.Command(bin, "overlay", "status")
	status.Dir, status.Env = repo.Root, repo.RunnerEnv()
	if out, err := status.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("upstream refs/remotes/origin/main conflict (blob ")) || !bytes.Contains(out, []byte("working file missing")) {
		t.Fatalf("overlay status missed failed pull: %v\n%s", err, out)
	}
	show := exec.Command(bin, "overlay", "show", "port")
	show.Dir, show.Env = repo.Root, repo.RunnerEnv()
	if saved, err := show.CombinedOutput(); err != nil || !bytes.Equal(saved, local) {
		t.Fatalf("saved patch unavailable after conflict: %v %q", err, saved)
	}
	showJSON := exec.Command(bin, "overlay", "show", "port", "--json")
	showJSON.Dir, showJSON.Env = repo.Root, repo.RunnerEnv()
	if out, err := showJSON.CombinedOutput(); err != nil {
		t.Fatalf("overlay show JSON: %v\n%s", err, out)
	} else {
		var result struct {
			Data struct {
				ContentBase64 string `json:"contentBase64"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(result.Data.ContentBase64)
		if err != nil || !bytes.Equal(decoded, local) {
			t.Fatalf("overlay show JSON bytes: %v %q", err, decoded)
		}
	}
	if got, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err == nil && (bytes.Contains(got, []byte("<<<<<<<")) || bytes.Contains(got, []byte("port = 9090"))) {
		t.Fatalf("conflicting pull wrote guessed content: %q", got)
	}
	remove := exec.Command(bin, "overlay", "remove", "port", "--json")
	remove.Dir, remove.Env = repo.Root, repo.RunnerEnv()
	if out, err := remove.CombinedOutput(); err != nil {
		t.Fatalf("remove after conflicting pull: %v\n%s", err, out)
	} else {
		var result struct {
			Data struct {
				State string `json:"state"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &result); err != nil || result.Data.State != "removed" {
			t.Fatalf("overlay remove JSON: %v\n%s", err, out)
		}
	}
	repo.Git("restore", "--", "app.conf")
	repo.Git("pull", "-q", "--ff-only", "origin", "main")
	if got := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(got, conflicting) {
		t.Fatalf("recovery did not advance mainline: %q", got)
	}
	resolved := bytes.Replace(conflicting, []byte("port = 9090"), []byte("port = 8080"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), resolved, 0600); err != nil {
		t.Fatal(err)
	}
	add = exec.Command(bin, "overlay", "add", "port", "--", "app.conf")
	add.Dir, add.Env = repo.Root, repo.RunnerEnv()
	if out, err := add.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("version 2")) {
		t.Fatalf("re-add resolved overlay: %v\n%s", err, out)
	}
	repo.Git("add", "app.conf")
	if staged := repo.Git("cat-file", "blob", ":app.conf"); !bytes.Equal(staged, conflicting) {
		t.Fatalf("resolved overlay entered index: %q", staged)
	}
	repo.Git("checkout", "-q", "--", "app.conf")
	if got, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(got, resolved) {
		t.Fatalf("resolved overlay was not restored: %v %q", err, got)
	}
}

func TestOverlayRebaseKeepsLocalPatchOutOfCommits(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := workspace.Init(context.Background(), r, repo.Root); err != nil {
		t.Fatal(err)
	}
	bin := integrationBinary(t)
	local := bytes.Replace(base, []byte("port = 80"), []byte("port = 8080"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), local, 0600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command(bin, "overlay", "add", "port", "--", "app.conf")
	add.Dir, add.Env = repo.Root, repo.RunnerEnv()
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("overlay add: %v\n%s", err, out)
	}
	repo.Git("add", "app.conf")
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	repo.Git("push", "-q", "-u", "origin", "main")
	other := filepath.Join(t.TempDir(), "other")
	repo.Git("clone", "-q", "-b", "main", remote, other)
	if err := os.WriteFile(filepath.Join(repo.Root, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "feature.txt")
	repo.Git("commit", "-qm", "local feature")
	upstream := bytes.Replace(base, []byte("log = info"), []byte("log = warn"), 1)
	if err := os.WriteFile(filepath.Join(other, "app.conf"), upstream, 0600); err != nil {
		t.Fatal(err)
	}
	otherGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command(repo.GitPath, args...)
		cmd.Dir, cmd.Env = other, repo.Env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("other git %v: %v\n%s", args, err, out)
		}
	}
	otherGit("add", "app.conf")
	otherGit("commit", "-qm", "upstream log")
	otherGit("push", "-q", "origin", "main")
	repo.Git("fetch", "-q", "origin")
	repo.Git("rebase", "origin/main")
	want := bytes.Replace(upstream, []byte("port = 80"), []byte("port = 8080"), 1)
	if got, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("rebase did not reapply overlay: %v %q", err, got)
	}
	if committed := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(committed, upstream) {
		t.Fatalf("rebase leaked overlay: %q", committed)
	}
	if feature := repo.Git("show", "HEAD:feature.txt"); !bytes.Equal(feature, []byte("feature\n")) {
		t.Fatalf("rebase lost ordinary feature: %q", feature)
	}
}

func TestOverlayWorktreesKeepIndependentPatches(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(context.Background(), r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	bin := integrationBinary(t)
	mainLocal := bytes.Replace(base, []byte("port = 80"), []byte("port = 8080"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), mainLocal, 0600); err != nil {
		t.Fatal(err)
	}
	local := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = dir, repo.RunnerEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git-local %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	local(repo.Root, "overlay", "add", "main-port", "--", "app.conf")
	repo.Git("add", "app.conf")
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "linked", linked)
	if got, err := os.ReadFile(filepath.Join(linked, "app.conf")); err != nil || !bytes.Equal(got, base) {
		t.Fatalf("new worktree should have clean mainline file: %v %q", err, got)
	}
	local(linked, "init")
	linkedLocal := bytes.Replace(base, []byte("port = 80"), []byte("port = 9090"), 1)
	if err := os.WriteFile(filepath.Join(linked, "app.conf"), linkedLocal, 0600); err != nil {
		t.Fatal(err)
	}
	local(linked, "overlay", "add", "linked-port", "--", "app.conf")
	repo.At(linked, "add", "app.conf")
	if staged := repo.At(linked, "show", ":app.conf"); !bytes.Equal(staged, base) {
		t.Fatalf("linked overlay entered index: %q", staged)
	}
	repo.Git("checkout", "-q", "--", "app.conf")
	repo.At(linked, "checkout", "-q", "--", "app.conf")
	if got, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(got, mainLocal) {
		t.Fatalf("main overlay changed: %v %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(linked, "app.conf")); err != nil || !bytes.Equal(got, linkedLocal) {
		t.Fatalf("linked overlay changed: %v %q", err, got)
	}
	identityPath := filepath.Join(w.Admin, "gitlocal-workspace.json")
	identityBytes, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(identityPath); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(mainLocal, []byte("log = info"), []byte("log = warn"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), changed, 0600); err != nil {
		t.Fatal(err)
	}
	brokenAdd := exec.Command(repo.GitPath, "add", "app.conf")
	brokenAdd.Dir, brokenAdd.Env = repo.Root, repo.Env
	if out, err := brokenAdd.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("identity is missing")) {
		t.Fatalf("missing registered identity should fail closed: %v\n%s", err, out)
	}
	if err := os.WriteFile(identityPath, identityBytes, 0600); err != nil {
		t.Fatal(err)
	}
	local(repo.Root, "overlay", "remove", "main-port")
	repo.At(linked, "checkout", "-q", "--", "app.conf")
	if got, err := os.ReadFile(filepath.Join(linked, "app.conf")); err != nil || !bytes.Equal(got, linkedLocal) {
		t.Fatalf("removing main overlay affected linked patch: %v %q", err, got)
	}
	local(linked, "overlay", "remove", "linked-port")
	if _, err := os.Stat(filepath.Join(w.Common, "info", "attributes")); !os.IsNotExist(err) {
		t.Fatalf("last removal did not remove newly created attributes file: %v", err)
	}
}

func TestOverlayGuardRejectsDirectIndexLeakAtCommitAndPush(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	bin := integrationBinary(t)
	localCommand := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git-local %v: %v\n%s", args, err, out)
		}
	}
	localCommand("init")
	patch := bytes.Replace(base, []byte("port = 80"), []byte("port = 8080"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	localCommand("overlay", "add", "port", "--", "app.conf")
	doctor := exec.Command(bin, "doctor")
	doctor.Dir, doctor.Env = repo.Root, repo.RunnerEnv()
	if out, err := doctor.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("pre-commit guard is not installed")) {
		t.Fatalf("doctor missed missing overlay guard: %v\n%s", err, out)
	}
	filterPath := filepath.Join(repo.Root, ".git", "gitlocal", filterConfigName)
	filterBytes, err := os.ReadFile(filterPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filterPath, []byte("[filter \"gitlocal-overlay\"]\n\tprocess = cat\n\trequired = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	doctor = exec.Command(bin, "doctor")
	doctor.Dir, doctor.Env = repo.Root, repo.RunnerEnv()
	if out, err := doctor.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("overlay filter configuration was changed")) {
		t.Fatalf("doctor missed damaged overlay filter: %v\n%s", err, out)
	}
	if err := os.WriteFile(filterPath, filterBytes, 0600); err != nil {
		t.Fatal(err)
	}
	localCommand("hook", "install")
	hash := exec.Command(repo.GitPath, "hash-object", "-w", "--stdin")
	hash.Dir, hash.Env, hash.Stdin = repo.Root, repo.Env, bytes.NewReader(patch)
	object, err := hash.CombinedOutput()
	if err != nil {
		t.Fatalf("hash overlay content: %v\n%s", err, object)
	}
	repo.Git("update-index", "--cacheinfo", "100644,"+string(bytes.TrimSpace(object))+",app.conf")
	commit := exec.Command(repo.GitPath, "commit", "-qm", "leak")
	commit.Dir, commit.Env = repo.Root, repo.Env
	if out, err := commit.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("overlay \"port\"")) {
		t.Fatalf("pre-commit missed direct index leak: %v\n%s", err, out)
	}
	if got := repo.Git("show", "HEAD:app.conf"); !bytes.Equal(got, base) {
		t.Fatalf("rejected commit changed HEAD: %q", got)
	}
	repo.Git("commit", "--no-verify", "-qm", "leak")
	remote := filepath.Join(t.TempDir(), "remote.git")
	repo.Git("init", "-q", "--bare", remote)
	repo.Git("remote", "add", "origin", remote)
	push := exec.Command(repo.GitPath, "push", "origin", "main")
	push.Dir, push.Env = repo.Root, repo.Env
	if out, err := push.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("overlay \"port\"")) {
		t.Fatalf("pre-push missed no-verify overlay leak: %v\n%s", err, out)
	}
	repo.Git("push", "--no-verify", "-u", "origin", "main")
	doctor = exec.Command(bin, "doctor")
	doctor.Dir, doctor.Env = repo.Root, repo.RunnerEnv()
	if out, err := doctor.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("overlay \"port\" on \"app.conf\" occurs in commit")) {
		t.Fatalf("doctor missed overlay leak in upstream history: %v\n%s", err, out)
	}
}

func TestOverlayRemoveRestoresGitFilesAndKeepsWorkingContent(t *testing.T) {
	repo := testutil.New(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\n")
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), base, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "initial")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := workspace.Init(context.Background(), r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	originalAttr := []byte("# user attributes\n*.md -text\n")
	if err := os.MkdirAll(filepath.Join(w.Common, "info"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Common, "info", "attributes"), originalAttr, 0640); err != nil {
		t.Fatal(err)
	}
	originalConfig, err := os.ReadFile(filepath.Join(w.Common, "config"))
	if err != nil {
		t.Fatal(err)
	}
	bin := integrationBinary(t)
	local := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = repo.Root, repo.RunnerEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git-local %v: %v\n%s", args, err, out)
		}
	}
	first := bytes.Replace(base, []byte("port = 80"), []byte("port = 8080"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), first, 0600); err != nil {
		t.Fatal(err)
	}
	local("overlay", "add", "port", "--", "app.conf")
	second := bytes.Replace(base, []byte("port = 80"), []byte("port = 9090"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), second, 0600); err != nil {
		t.Fatal(err)
	}
	local("overlay", "add", "port", "--", "app.conf")
	local("overlay", "remove", "port")
	if got, err := os.ReadFile(filepath.Join(repo.Root, "app.conf")); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("remove changed working content: %v %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(w.Common, "info", "attributes")); err != nil || !bytes.Equal(got, originalAttr) {
		t.Fatalf("remove did not restore user attributes: %v %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(w.Common, "config")); err != nil || !bytes.Equal(got, originalConfig) {
		t.Fatalf("remove did not restore Git config bytes: %v\nwant: %q\ngot: %q", err, originalConfig, got)
	}
	if _, err := os.Stat(filepath.Join(w.Common, "gitlocal", filterConfigName)); !os.IsNotExist(err) {
		t.Fatalf("owner config file still installed: %v", err)
	}
	if v, err := LoadVersion(context.Background(), w.Common, w.WorkspaceID, "port", 1); err != nil || !bytes.Equal(v.Local, first) {
		t.Fatalf("removed overlay lost older version: %v %q", err, v.Local)
	}
	repo.Git("add", "app.conf")
	if staged := repo.Git("cat-file", "blob", ":app.conf"); !bytes.Equal(staged, second) {
		t.Fatalf("former overlay did not become ordinary edit: %q", staged)
	}
	third := bytes.Replace(base, []byte("port = 80"), []byte("port = 7070"), 1)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), third, 0600); err != nil {
		t.Fatal(err)
	}
	local("overlay", "add", "port", "--", "app.conf")
	if v, err := LoadVersion(context.Background(), w.Common, w.WorkspaceID, "port", 0); err != nil || v.Manifest.Version != 3 || !bytes.Equal(v.Local, third) {
		t.Fatalf("re-added overlay did not advance retained history: %v %+v", err, v.Manifest)
	}
	showOld := exec.Command(bin, "overlay", "show", "port", "--version", "1")
	showOld.Dir, showOld.Env = repo.Root, repo.RunnerEnv()
	if shown, err := showOld.CombinedOutput(); err != nil || !bytes.Equal(shown, first) {
		t.Fatalf("historical overlay show: %v %q", err, shown)
	}
	version, err := LoadVersion(context.Background(), w.Common, w.WorkspaceID, "port", 0)
	if err != nil {
		t.Fatal(err)
	}
	version.Manifest.State = "removing"
	manifestData, err := json.Marshal(version.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(w.Common, "gitlocal", "overlays", w.WorkspaceID, "port", "manifest.json")
	if err := os.WriteFile(manifestPath, append(manifestData, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Common, "info", "attributes"), originalAttr, 0640); err != nil {
		t.Fatal(err)
	}
	local("overlay", "remove", "port")
	if got, err := os.ReadFile(filepath.Join(w.Common, "info", "attributes")); err != nil || !bytes.Equal(got, originalAttr) {
		t.Fatalf("second removal changed user attributes: %v %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(w.Common, "config")); err != nil || !bytes.Equal(got, originalConfig) {
		t.Fatalf("second removal changed Git config: %v", err)
	}
}
