package selection

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
)

func fixture(t *testing.T) (*testutil.Repo, *Engine) {
	t.Helper()
	r := testutil.New(t)
	g, err := gitexec.New(r.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(g, r.Root, filepath.Join(r.Root, ".git"))
	if err != nil {
		g.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(); g.Close() })
	return r, e
}
func write(t *testing.T, r *testutil.Repo, path string) {
	t.Helper()
	p := filepath.Join(r.Root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("fixture\r\n\x00\xff"), 0600); err != nil {
		t.Fatal(err)
	}
}
func paths(p Plan) []string {
	out := []string{}
	for _, e := range p.Entries {
		out = append(out, e.Path)
	}
	return out
}
func TestLiteralPathsAndFutureExclusions(t *testing.T) {
	r, e := fixture(t)
	ctx := context.Background()
	names := []string{"notes/private.txt", "notes/space name", "notes/café", "notes/-dash"}
	if runtime.GOOS != "windows" {
		names = append(names, "notes/with\ttab", "notes/literal*?[x]")
	}
	for _, p := range names {
		write(t, r, p)
	}
	p, err := e.PlanTrack(ctx, Empty(), r.Root, "notes/")
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.PlanUntrack(p.State, r.Root, "notes/private.txt")
	if err != nil {
		t.Fatal(err)
	}
	write(t, r, "notes/future")
	p, err = e.Validate(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths(p) {
		if path == "notes/private.txt" {
			t.Fatal("captured excluded file")
		}
	}
	if !s.Selected("notes/future") {
		t.Fatal("lost future-file semantics")
	}
	os.Remove(filepath.Join(r.Root, "notes/private.txt"))
	os.Mkdir(filepath.Join(r.Root, "notes/private.txt"), 0700)
	write(t, r, "notes/private.txt/never")
	if _, err = e.Validate(ctx, s); err != nil {
		t.Fatal("excluded replacement subtree should be pruned", err)
	}
	p, err = e.PlanTrack(ctx, s, r.Root, "notes/private.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !p.State.Selected("notes/private.txt/never") {
		t.Fatal("exact retracking failed")
	}
}
func TestNormalizationAndDeletion(t *testing.T) {
	r, e := fixture(t)
	write(t, r, "dir/file")
	ctx := context.Background()
	p, err := e.PlanTrack(ctx, Empty(), filepath.Join(r.Root, "dir"), "./file")
	if err != nil || p.State.Include[0].Path != "dir/file" {
		t.Fatal(p, err)
	}
	for _, arg := range []string{"..", "../file", "dir/../file", r.Root, ".git/config", ""} {
		if _, err = e.Normalize(r.Root, arg); err == nil {
			t.Fatalf("accepted %q", arg)
		}
	}
	if _, err = e.Normalize(r.Root, "."); err == nil {
		t.Fatal("selected checkout root")
	}
	os.Remove(filepath.Join(r.Root, "dir/file"))
	p, err = e.Validate(ctx, p.State)
	if err != nil || len(p.Missing) != 1 || len(p.Entries) != 0 {
		t.Fatal("lost deleted selection", err)
	}
	if _, err = e.PlanTrack(ctx, Empty(), r.Root, "dir/file"); err == nil {
		t.Fatal("new missing path selected")
	}
	os.Mkdir(filepath.Join(r.Root, "dir/file"), 0700)
	if _, err = e.Validate(ctx, p.State); err == nil {
		t.Fatal("file selection broadened into directory")
	}
}
func TestSymlinksHardlinksAndNestedRepositories(t *testing.T) {
	r, e := fixture(t)
	write(t, r, "notes/plain")
	ctx := context.Background()
	t.Run("hardlink", func(t *testing.T) {
		if err := os.Link(filepath.Join(r.Root, "notes/plain"), filepath.Join(r.Root, "alias")); err != nil {
			t.Skip(err)
		}
		defer os.Remove(filepath.Join(r.Root, "alias"))
		if _, err := e.PlanTrack(ctx, Empty(), r.Root, "notes"); err == nil {
			t.Fatal("accepted hardlink")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(r.Root, "link")); err != nil {
			t.Skip(err)
		}
		defer os.Remove(filepath.Join(r.Root, "link"))
		p, err := e.PlanTrack(ctx, Empty(), r.Root, "link")
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Entries) != 1 || p.Entries[0].LinkTarget != outside {
			t.Fatal("link was dereferenced")
		}
		if _, err = e.PlanTrack(ctx, Empty(), r.Root, "link/child"); err == nil {
			t.Fatal("followed directory symlink")
		}
	})
	t.Run("nested", func(t *testing.T) {
		nested := filepath.Join(r.Root, "notes/nested")
		r.Git("init", "-q", nested)
		write(t, r, "notes/nested/local")
		if _, err := e.PlanTrack(ctx, Empty(), r.Root, "notes/nested/local"); err == nil {
			t.Fatal("selected inside nested repository")
		}
		if _, err := e.PlanTrack(ctx, Empty(), r.Root, "notes"); err == nil {
			t.Fatal("traversed nested repository")
		}
		s := mustCanonical(t, State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes/nested"}})
		if _, err := e.Validate(ctx, s); err != nil {
			t.Fatal("excluded nested repository not pruned", err)
		}
	})
	t.Run("bare", func(t *testing.T) {
		bare := filepath.Join(r.Root, "bare")
		r.Git("init", "--bare", "-q", bare)
		if _, err := e.PlanTrack(ctx, Empty(), r.Root, "bare"); err == nil {
			t.Fatal("selected bare repository")
		}
	})
}
func TestOwnedPathsAndExplicitExclusions(t *testing.T) {
	r, e := fixture(t)
	r.Commit()
	ctx := context.Background()
	if _, err := e.PlanTrack(ctx, Empty(), r.Root, "app.txt"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("tracked file accepted", err)
	}
	r.Git("rm", "--cached", "app.txt")
	if _, err := e.PlanTrack(ctx, Empty(), r.Root, "app.txt"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("staged deletion accepted", err)
	}
	write(t, r, "notes/shared")
	write(t, r, "notes/private")
	r.Git("add", "notes/shared")
	if _, err := e.PlanTrack(ctx, Empty(), r.Root, "notes"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("tracked descendant accepted", err)
	}
	s := mustCanonical(t, State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}})
	// Metadata-only untrack remains possible even with owned/unsafe paths.
	s, err := e.PlanUntrack(s, r.Root, "notes/shared")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Validate(ctx, s)
	if err != nil || strings.Join(paths(p), ",") != "notes/private" {
		t.Fatal("excluded owned descendant blocked unrelated files", err)
	}
	r.Git("add", "-N", "notes/private")
	if _, err = e.Validate(ctx, s); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("intent-to-add not owned", err)
	}
}
func TestCaseAliasesAndUnexpectedNames(t *testing.T) {
	r, e := fixture(t)
	ctx := context.Background()
	write(t, r, "notes/Secret")
	s := mustCanonical(t, State{SchemaVersion: 2, Include: []Rule{{"notes", "directory"}}, Exclude: []string{"notes/secret"}})
	if _, err := e.Validate(ctx, s); err == nil {
		t.Fatal("alias bypassed exclusion")
	}
	// Windows can't hold a newline in a filename.
	if runtime.GOOS != "windows" {
		write(t, r, "notes/unsafe\nname")
		if _, err := e.PlanTrack(ctx, Empty(), r.Root, "notes"); err == nil {
			t.Fatal("accepted unsupported filename")
		}
	}
	r.Commit()
	if _, err := e.PlanTrack(ctx, Empty(), r.Root, "APP.TXT"); err == nil {
		t.Fatal("case alias of owned file accepted")
	}
}
func TestValidationLeavesMainStateUnchanged(t *testing.T) {
	r, e := fixture(t)
	r.Commit()
	write(t, r, "notes/file")
	beforeIndex, _ := os.ReadFile(filepath.Join(r.Root, ".git/index"))
	beforeRefs := r.Git("show-ref")
	beforeObjects := r.Git("count-objects", "-v")
	beforeExclude, _ := os.ReadFile(filepath.Join(r.Root, ".git/info/exclude"))
	p, err := e.PlanTrack(context.Background(), Empty(), r.Root, "notes")
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.PlanUntrack(p.State, r.Root, "notes/file")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Validate(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	afterIndex, _ := os.ReadFile(filepath.Join(r.Root, ".git/index"))
	afterExclude, _ := os.ReadFile(filepath.Join(r.Root, ".git/info/exclude"))
	if !bytes.Equal(beforeIndex, afterIndex) || !bytes.Equal(beforeRefs, r.Git("show-ref")) || !bytes.Equal(beforeObjects, r.Git("count-objects", "-v")) || !bytes.Equal(beforeExclude, afterExclude) {
		t.Fatal("selection validation changed main repository")
	}
	if _, err = os.Stat(filepath.Join(r.Root, ".git/gitlocal")); !os.IsNotExist(err) {
		t.Fatal("planning persisted configuration")
	}
}

func TestSparseIndexAndSkipWorktreeOwnership(t *testing.T) {
	r, e := fixture(t)
	write(t, r, "visible/one")
	write(t, r, "hidden/two")
	r.Git("add", ".")
	r.Git("commit", "-qm", "directories")
	r.Git("sparse-checkout", "init", "--cone", "--sparse-index")
	r.Git("sparse-checkout", "set", "visible")
	raw := r.Git("ls-files", "--sparse", "--stage")
	if !bytes.Contains(raw, []byte("040000")) {
		t.Fatal("fixture has no sparse directory entry")
	}
	before, _ := os.ReadFile(filepath.Join(r.Root, ".git/index"))
	s := mustCanonical(t, State{SchemaVersion: 2, Include: []Rule{{"hidden/two", "file"}}})
	if _, err := e.Validate(context.Background(), s); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("sparse-owned absent file accepted", err)
	}
	after, _ := os.ReadFile(filepath.Join(r.Root, ".git/index"))
	if !bytes.Equal(before, after) {
		t.Fatal("ownership inspection rewrote sparse index")
	}
	r.Git("sparse-checkout", "disable")
	r.Git("update-index", "--skip-worktree", "hidden/two")
	os.Remove(filepath.Join(r.Root, "hidden/two"))
	if _, err := e.Validate(context.Background(), s); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("skip-worktree file accepted", err)
	}
}
func TestEveryUnmergedStageIsOwned(t *testing.T) {
	r, e := fixture(t)
	r.Commit()
	oid := strings.TrimSpace(string(r.Git("rev-parse", "HEAD:app.txt")))
	for _, stage := range []string{"1", "2", "3"} {
		path := "conflict-" + stage
		write(t, r, path)
		cmd := exec.Command(r.GitPath, "update-index", "--index-info")
		cmd.Dir = r.Root
		cmd.Env = r.Env
		cmd.Stdin = strings.NewReader("100644 " + oid + " " + stage + "\t" + path + "\n")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, b)
		}
		if _, err := e.PlanTrack(context.Background(), Empty(), r.Root, path); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("stage %s accepted: %v", stage, err)
		}
	}
}
func TestCorruptIndexAndMissingTreesFailClosed(t *testing.T) {
	t.Run("index", func(t *testing.T) {
		r, e := fixture(t)
		r.Commit()
		write(t, r, "local")
		os.WriteFile(filepath.Join(r.Root, ".git/index"), []byte("corrupt"), 0600)
		if _, err := e.PlanTrack(context.Background(), Empty(), r.Root, "local"); err == nil {
			t.Fatal("corrupt index treated as empty")
		}
	})
	t.Run("partial", func(t *testing.T) {
		r, e := fixture(t)
		r.Commit()
		r.Git("config", "uploadpack.allowFilter", "true")
		clone := filepath.Join(r.Root, "partial")
		r.Git("clone", "-q", "--no-local", "--no-checkout", "--filter=tree:0", r.Root, clone)
		partial, err := New(e.git, clone, filepath.Join(clone, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		defer partial.Close()
		os.WriteFile(filepath.Join(clone, "local"), []byte("local"), 0600)
		before := r.At(clone, "count-objects", "-v")
		if _, err = partial.PlanTrack(context.Background(), Empty(), clone, "local"); err == nil {
			t.Fatal("missing ownership trees treated as empty")
		}
		if !bytes.Equal(before, r.At(clone, "count-objects", "-v")) {
			t.Fatal("ownership fetched objects")
		}
	})
}
func TestOwnershipRevalidationAndReplacementRefs(t *testing.T) {
	r, e := fixture(t)
	r.Commit()
	ctx := context.Background()
	owner, err := InspectOwnership(ctx, e.git, r.Root, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	write(t, r, "addition")
	r.Git("add", "addition")
	if err = owner.Revalidate(ctx, e.git, r.Root, e.admin); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("index change not detected", err)
	}
	main := strings.TrimSpace(string(r.Git("rev-parse", "HEAD")))
	empty := strings.TrimSpace(string(r.Git("mktree")))
	replacement := strings.TrimSpace(string(r.Git("commit-tree", empty, "-m", "replacement")))
	r.Git("replace", main, replacement)
	if _, err = e.PlanTrack(ctx, Empty(), r.Root, "app.txt"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("replacement hid ownership", err)
	}
}
func TestSubmoduleAndMetadataOnlyUntrack(t *testing.T) {
	source := testutil.New(t)
	source.Commit()
	r, e := fixture(t)
	r.Commit()
	r.Git("-c", "protocol.file.allow=always", "submodule", "add", "-q", source.Root, "module")
	s := mustCanonical(t, State{SchemaVersion: 2, Include: []Rule{{"module/local", "file"}}})
	if _, err := e.Validate(context.Background(), s); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("gitlink ancestor not owned", err)
	}
	// This path no longer exists and its ancestor is main-owned. No filesystem or
	// ownership traversal should be necessary to stop selecting it.
	s, err := e.PlanUntrack(s, r.Root, "module/local")
	if err != nil || len(s.Include) != 0 {
		t.Fatal("could not untrack conflicting selection", err)
	}
}
func TestMultiArgumentGeneration(t *testing.T) {
	r, e := fixture(t)
	write(t, r, "one")
	write(t, r, "two")
	s := Empty()
	s.Generation = math.MaxUint64 - 1
	p, err := e.PlanTrack(context.Background(), s, r.Root, "one", "two")
	if err != nil || p.State.Generation != math.MaxUint64 {
		t.Fatal("one command consumed multiple generations", err)
	}
}
func TestHooksAndFiltersAreNeverInvoked(t *testing.T) {
	r, e := fixture(t)
	write(t, r, "notes/data")
	marker := filepath.Join(r.Root, "executed")
	command := testutil.HostileCommand(t, "TestHostileGitProgram", marker)
	r.Git("config", "filter.hostile.clean", command)
	if err := os.WriteFile(filepath.Join(r.Root, ".gitattributes"), []byte("notes/* filter=hostile\n"), 0600); err != nil {
		t.Fatal(err)
	}
	write(t, r, "notes/probe")
	r.Git("add", "notes/probe")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("clean filter control did not execute: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	r.Git("rm", "-q", "--cached", "--", "notes/probe")
	if err := os.Remove(filepath.Join(r.Root, "notes", "probe")); err != nil {
		t.Fatal(err)
	}
	r.Git("config", "core.fsmonitor", command)
	r.Git("status", "--short")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fsmonitor control did not execute: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanTrack(context.Background(), Empty(), r.Root, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "executed")); !os.IsNotExist(err) {
		t.Fatal("executed configured hook/filter")
	}
}

func TestHostileGitProgram(t *testing.T) {
	testutil.RunHostileProgram()
}

func TestCorruptHeadIsNotAnUnbornBranch(t *testing.T) {
	r, e := fixture(t)
	r.Commit()
	write(t, r, "local")
	if err := os.WriteFile(filepath.Join(r.Root, ".git/refs/heads/main"), []byte("not-an-object-id\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanTrack(context.Background(), Empty(), r.Root, "local"); err == nil {
		t.Fatal("corrupt HEAD treated as unborn")
	}
}
