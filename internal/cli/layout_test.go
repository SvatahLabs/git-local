package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// layoutRepo is an initialized main checkout with an isolated HOME.
func layoutRepo(t *testing.T) *testutil.Repo {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, home)
	successful(t, repo.Root, "init")
	return repo
}

type listedWorkspace struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Root       string `json:"root"`
	Registered bool   `json:"registered"`
	Available  bool   `json:"available"`
}

func listWorkspaces(t *testing.T, cwd string) []listedWorkspace {
	t.Helper()
	out := successful(t, cwd, "workspace", "list", "--json")
	var envelope struct {
		Data struct {
			Workspaces []listedWorkspace `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("workspace list JSON: %v\n%s", err, out)
	}
	return envelope.Data.Workspaces
}

func doctorReportsNoErrors(t *testing.T, cwd string) {
	t.Helper()
	code, out, diagnostic := callCLI(t, cwd, "doctor", "--json")
	var envelope struct {
		Data struct {
			Findings []struct {
				Severity string `json:"severity"`
				Check    string `json:"check"`
				Message  string `json:"message"`
			} `json:"findings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("doctor JSON (exit %d): %v\n%s%s", code, err, out, diagnostic)
	}
	for _, finding := range envelope.Data.Findings {
		if finding.Severity == "error" {
			t.Fatalf("doctor error: %+v\n%s", finding, out)
		}
	}
}

// TestRelativeWorktreePathsWork: Git 2.48+ can record a linked worktree's
// gitdir pointer as a relative path. Lifecycle commands and doctor must
// resolve it.
func TestRelativeWorktreePathsWork(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "--relative-paths", "-b", "relative", linked)
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(repo.Root, ".git", "worktrees"))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	pointer, err := os.ReadFile(filepath.Join(repo.Root, ".git", "worktrees", entries[0].Name(), "gitdir"))
	if err != nil || filepath.IsAbs(strings.TrimSpace(string(pointer))) {
		t.Fatalf("Git didn't record a relative gitdir pointer: %q %v", pointer, err)
	}
	if got := listWorkspaces(t, repo.Root); len(got) != 1 {
		t.Fatalf("workspaces before linked init: %+v", got)
	}
	successful(t, linked, "init")
	listed := listWorkspaces(t, repo.Root)
	if len(listed) != 2 {
		t.Fatalf("workspaces after linked init: %+v", listed)
	}
	for _, w := range listed {
		if !w.Registered || !w.Available {
			t.Fatalf("workspace not registered and available: %+v", w)
		}
	}
	doctorReportsNoErrors(t, repo.Root)
	doctorReportsNoErrors(t, linked)
	successful(t, repo.Root, "workspace", "prune")
	successful(t, repo.Root, "workspace", "remove", linked)
	for _, w := range listWorkspaces(t, repo.Root) {
		if w.Root == linked && w.State != "retained" {
			t.Fatalf("removed workspace not retained: %+v", w)
		}
	}
}

// linkedWorkspace adds and initializes a linked worktree at dir/linked that
// selects its own notes.local.
func linkedWorkspace(t *testing.T, repo *testutil.Repo, dir, branch string) string {
	t.Helper()
	linked := filepath.Join(dir, "linked")
	repo.Git("worktree", "add", "-q", "-b", branch, linked)
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	successful(t, linked, "init")
	if err := os.WriteFile(filepath.Join(linked, "notes.local"), []byte("linked notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, linked, "track", "notes.local")
	return linked
}

// TestOfflineWorktreeIsUnavailableNotFatal: a worktree on an offline volume
// fails stat with errors other than ENOENT or EACCES. Here its parent is
// replaced by a file, so stat reports ENOTDIR. The other checkouts keep
// working and are warned.
func TestOfflineWorktreeIsUnavailableNotFatal(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(volume, 0700); err != nil {
		t.Fatal(err)
	}
	linked := linkedWorkspace(t, repo, volume, "offline")
	if err := os.Rename(volume, volume+".away"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(volume, []byte("not a directory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(linked); err == nil || os.IsNotExist(err) || os.IsPermission(err) {
		t.Fatalf("stat error is %v, want neither ENOENT nor EACCES", err)
	}
	unavailable := false
	for _, w := range listWorkspaces(t, repo.Root) {
		unavailable = unavailable || w.Root == linked && !w.Available && w.State == "unavailable"
	}
	if !unavailable {
		t.Fatalf("workspace list: %+v", listWorkspaces(t, repo.Root))
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "main.local"), []byte("main notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, diagnostic := callCLI(t, repo.Root, "track", "main.local")
	if code != 0 || !strings.Contains(diagnostic, "unavailable") {
		t.Fatalf("track beside an offline worktree: exit %d: %s", code, diagnostic)
	}
	code, _, diagnostic = callCLI(t, repo.Root, "init")
	if code != 0 || !strings.Contains(diagnostic, linked) || !strings.Contains(diagnostic, "unavailable") {
		t.Fatalf("init beside an offline worktree: exit %d: %s", code, diagnostic)
	}
	successful(t, repo.Root, "commit", "-m", "main notes")
	successful(t, repo.Root, "status")
	doctorReportsNoErrors(t, repo.Root)
	// The volume comes back.
	if err := os.Remove(volume); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(volume+".away", volume); err != nil {
		t.Fatal(err)
	}
	successful(t, linked, "commit", "-m", "linked notes")
	for _, w := range listWorkspaces(t, repo.Root) {
		if !w.Available {
			t.Fatalf("workspace still unavailable: %+v", w)
		}
	}
}

// TestRegisteredWorktreeThatIsNoCheckout: a registered worktree whose .git
// file was deleted still exists as a directory. Init elsewhere skips it with
// a warning naming the fix, unless it holds a git-local workspace.
func TestRegisteredWorktreeThatIsNoCheckout(t *testing.T) {
	t.Parallel()
	repo := layoutRepo(t)
	plain := filepath.Join(t.TempDir(), "plain")
	repo.Git("worktree", "add", "-q", "-b", "plain", plain)
	plain, err := filepath.EvalSymlinks(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plain, ".git")); err != nil {
		t.Fatal(err)
	}
	code, _, diagnostic := callCLI(t, repo.Root, "init")
	if code != 0 || !strings.Contains(diagnostic, plain) || !strings.Contains(diagnostic, "git worktree repair "+plain) {
		t.Fatalf("init beside a worktree with no .git file: exit %d: %s", code, diagnostic)
	}
	linked := linkedWorkspace(t, repo, t.TempDir(), "identified")
	if err := os.Remove(filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	code, _, diagnostic = callCLI(t, repo.Root, "init")
	if code != 3 || !strings.Contains(diagnostic, linked) || !strings.Contains(diagnostic, "git worktree repair "+linked) {
		t.Fatalf("init beside an identified worktree with no .git file: exit %d: %s", code, diagnostic)
	}
	// Git repairs the .git file but exits 1, reporting it was broken.
	repair := exec.Command(repo.GitPath, "worktree", "repair", linked)
	repair.Dir, repair.Env = repo.Root, repo.Env
	if out, err := repair.CombinedOutput(); err != nil && !strings.Contains(string(out), ".git file broken") {
		t.Fatalf("git worktree repair: %v\n%s", err, out)
	}
	successful(t, repo.Root, "init")
	successful(t, linked, "status")
}

// TestUnreadableWorktreeRegistrationStopsInit: init can't tell whether a
// registered worktree that is no checkout holds a workspace when its gitdir
// file can't be read, so it stops and names the file instead of skipping it.
func TestUnreadableWorktreeRegistrationStopsInit(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	repo := layoutRepo(t)
	linked := linkedWorkspace(t, repo, t.TempDir(), "unreadable")
	if err := os.Remove(filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	gitdir := strings.TrimSpace(string(repo.Git("rev-parse", "--git-common-dir")))
	admins, err := filepath.Glob(filepath.Join(repo.Root, gitdir, "worktrees", "*", "gitdir"))
	if err != nil || len(admins) != 1 {
		t.Fatalf("worktree registrations: %v %v", admins, err)
	}
	gitdir = admins[0]
	if err := os.Chmod(gitdir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(gitdir, 0600) })
	code, _, diagnostic := callCLI(t, repo.Root, "init")
	if code != 3 || !strings.Contains(diagnostic, gitdir) {
		t.Fatalf("init beside a worktree with an unreadable gitdir: exit %d: %s", code, diagnostic)
	}
	if err := os.Chmod(gitdir, 0600); err != nil {
		t.Fatal(err)
	}
	code, _, diagnostic = callCLI(t, repo.Root, "init")
	if code != 3 || !strings.Contains(diagnostic, "git worktree repair "+linked) {
		t.Fatalf("init once the gitdir is readable: exit %d: %s", code, diagnostic)
	}
}

// TestStrayFileInWorktreesDirectoryIsIgnored: Git ignores files such as
// .DS_Store under .git/worktrees, and so must init.
func TestStrayFileInWorktreesDirectoryIsIgnored(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	home := filepath.Join(repo.Root, "fixture-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, home)
	common := strings.TrimSpace(string(repo.Git("rev-parse", "--git-common-dir")))
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo.Root, common)
	}
	worktrees := filepath.Join(common, "worktrees")
	if err := os.MkdirAll(worktrees, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktrees, ".DS_Store"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, repo.Root, "init")
	if _, err := os.Stat(filepath.Join(common, "gitlocal", "repository.json")); err != nil {
		t.Fatalf("init beside a stray file in .git/worktrees: %v", err)
	}
}

// TestBareRepositoryWorktreesRetire: Git lists a bare repository first among
// its worktrees, though it isn't one. Removed linked worktrees must still be
// retired, and --from main must say there is no main checkout.
func TestBareRepositoryWorktreesRetire(t *testing.T) {
	t.Parallel()
	seed := testutil.New(t)
	seed.Commit()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(dir, "repo.git")
	seed.Git("clone", "-q", "--bare", seed.Root, bare)
	add := func(name string, args ...string) string {
		path := filepath.Join(dir, name)
		seed.At(bare, append([]string{"worktree", "add", "-q"}, append(args, path)...)...)
		return path
	}
	one := add("one", "-b", "one")
	two := add("two", "-b", "two")
	successful(t, one, "init")
	successful(t, two, "init")
	if err := os.WriteFile(filepath.Join(two, "notes.local"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	successful(t, two, "track", "notes.local")
	var twoID string
	for _, w := range listWorkspaces(t, one) {
		if w.Root == two {
			twoID = w.ID
		}
	}
	if len(listWorkspaces(t, one)) != 2 || twoID == "" {
		t.Fatalf("workspaces: %+v", listWorkspaces(t, one))
	}
	seed.At(bare, "worktree", "remove", "--force", two)
	if out := successful(t, one, "workspace", "prune", "--dry-run"); !strings.Contains(out, twoID) {
		t.Fatalf("prune --dry-run: %s", out)
	}
	successful(t, one, "workspace", "retire", twoID)
	successful(t, one, "workspace", "retire", twoID)
	successful(t, one, "workspace", "prune")
	for _, w := range listWorkspaces(t, one) {
		if w.ID == twoID && w.State != "retained" {
			t.Fatalf("removed workspace: %+v", w)
		}
	}
	three := add("three", "-b", "three")
	successful(t, three, "init")
	successful(t, one, "workspace", "remove", three)
	four := add("four", "-b", "four")
	code, _, diagnostic := callCLI(t, four, "init", "--from", "main")
	if code == 0 || !strings.Contains(diagnostic, "bare") {
		t.Fatalf("init --from main in a bare repository's worktree: exit %d: %s", code, diagnostic)
	}
	doctorReportsNoErrors(t, one)
}
