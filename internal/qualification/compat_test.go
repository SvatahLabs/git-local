package qualification

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlocal/internal/configtx"
	"gitlocal/internal/doctor"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// These fixtures exercise real native Git index and object formats. A private
// snapshot must never rewrite the main index or advance its HEAD.
func TestNativeRepositoryFormatsKeepMainlineUntouched(t *testing.T) {
	for _, mode := range []string{"sha1", "sha256", "split-index", "sparse-index", "submodule", "unborn"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repo := testutil.New(t)
			if mode == "sha256" {
				if err := os.RemoveAll(filepath.Join(repo.Root, ".git")); err != nil {
					t.Fatal(err)
				}
				repo.Git("init", "-q", "-b", "main", "--object-format=sha256", "--template=")
			}
			if mode != "unborn" {
				if mode == "sparse-index" {
					for _, path := range []string{"src/app.txt", "other/app.txt"} {
						if err := os.MkdirAll(filepath.Dir(filepath.Join(repo.Root, path)), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(repo.Root, path), []byte(path+"\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					repo.Git("add", "src/app.txt", "other/app.txt")
					repo.Git("commit", "-qm", "initial")
					repo.Git("sparse-checkout", "init", "--cone", "--sparse-index")
					repo.Git("sparse-checkout", "set", "src")
				} else if mode == "submodule" {
					source := testutil.New(t)
					source.Commit()
					repo.Git("-c", "protocol.file.allow=always", "submodule", "add", "-q", source.Root, "vendor/sub")
					repo.Git("commit", "-qm", "add submodule")
				} else {
					repo.Commit()
				}
			}
			if mode == "split-index" {
				repo.Git("update-index", "--split-index")
			}
			beforeIndex := repo.Git("ls-files", "--stage", "-z")
			var beforeHead []byte
			if mode != "unborn" {
				beforeHead = repo.Git("rev-parse", "--verify", "--quiet", "HEAD")
			}
			r, err := gitexec.New(repo.RunnerEnv())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			w, err := workspace.Init(ctx, r, repo.Root)
			if err != nil {
				t.Fatal(err)
			}
			localPath := "notes/private.txt"
			if mode == "sparse-index" {
				localPath = "src/private.txt"
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(repo.Root, localPath)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo.Root, localPath), []byte("local only\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := configtx.Change(ctx, r, repo.Root, true, localPath); err != nil {
				t.Fatal(err)
			}
			if _, err := history.Commit(ctx, r, repo.Root, "native format qualification"); err != nil {
				t.Fatal(err)
			}
			if got := repo.Git("ls-files", "--stage", "-z"); !bytes.Equal(got, beforeIndex) {
				t.Fatal("private snapshot changed main index")
			}
			if mode != "unborn" {
				if got := repo.Git("rev-parse", "--verify", "--quiet", "HEAD"); !bytes.Equal(got, beforeHead) {
					t.Fatal("private snapshot advanced main HEAD")
				}
			}
			log, err := history.Log(ctx, r, repo.Root, 10)
			if err != nil || len(log) != 1 {
				t.Fatalf("snapshot unavailable: %+v %v", log, err)
			}
			content, err := history.Show(ctx, r, repo.Root, "", "latest", localPath)
			if err != nil || !bytes.Equal(content, []byte("local only\n")) {
				t.Fatalf("snapshot content unavailable: %q %v", content, err)
			}
			report, err := doctor.Run(ctx, r, repo.Root)
			if err != nil || report.HasIssues() || report.WorkspaceID != w.WorkspaceID {
				t.Fatalf("doctor rejected native format: %v\n%s", err, report.Human())
			}
		})
	}
}

func TestPartialCloneWithMissingObjectsReportsOfflineConflict(t *testing.T) {
	ctx := context.Background()
	repo := testutil.New(t)
	repo.Commit()
	repo.Git("config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "partial")
	repo.Git("clone", "-q", "--no-local", "--no-checkout", "--filter=tree:0", repo.Root, clone)
	var err error
	clone, err = filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	repo.At(clone, "config", "remote.origin.url", "hostile::never")
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := workspace.Init(ctx, r, clone); err != nil {
		t.Fatal(err)
	}
	before := repo.At(clone, "count-objects", "-v")
	report, err := doctor.Run(ctx, r, clone)
	if err != nil || !report.HasIssues() || !strings.Contains(report.Human(), "git ls-tree failed") {
		t.Fatalf("missing objects were not reported: %v\n%s", err, report.Human())
	}
	if got := repo.At(clone, "count-objects", "-v"); !bytes.Equal(got, before) {
		t.Fatal("qualification read fetched missing objects")
	}
}

func TestConcurrentSnapshotsSerializeWithoutChangingMainline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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
	for i := 0; i < 64; i++ {
		path := filepath.Join(repo.Root, "notes", fmt.Sprintf("%02d.txt", i))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf("local %d\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "notes"); err != nil {
		t.Fatal(err)
	}
	otherRunner, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer otherRunner.Close()
	beforeIndex := repo.Git("ls-files", "--stage", "-z")
	beforeHead := repo.Git("rev-parse", "HEAD")
	type outcome struct {
		result history.Result
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	for _, runner := range []*gitexec.Runner{r, otherRunner} {
		workers.Add(1)
		go func(runner *gitexec.Runner) {
			defer workers.Done()
			<-start
			result, err := history.Commit(ctx, runner, repo.Root, "concurrent qualification")
			results <- outcome{result, err}
		}(runner)
	}
	close(start)
	workers.Wait()
	close(results)
	changed := 0
	var snapshot string
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.result.Changed {
			changed++
		}
		if snapshot == "" {
			snapshot = got.result.ID
		} else if got.result.ID != snapshot {
			t.Fatalf("concurrent snapshots diverged: %q and %q", snapshot, got.result.ID)
		}
	}
	if changed != 1 || snapshot == "" {
		t.Fatalf("expected exactly one published snapshot, got %d with id %q", changed, snapshot)
	}
	log, err := history.Log(ctx, r, repo.Root, 10)
	if err != nil || len(log) != 1 || log[0].ID != snapshot {
		t.Fatalf("concurrent history invalid: %+v %v", log, err)
	}
	if got := repo.Git("ls-files", "--stage", "-z"); !bytes.Equal(got, beforeIndex) {
		t.Fatal("concurrent snapshots changed the main index")
	}
	if got := repo.Git("rev-parse", "HEAD"); !bytes.Equal(got, beforeHead) {
		t.Fatal("concurrent snapshots changed main HEAD")
	}
}

func TestHostileGitConfigurationCannotExecuteDuringLocalCommands(t *testing.T) {
	ctx := context.Background()
	repo := testutil.New(t)
	repo.Commit()
	attributes := filepath.Join(repo.Root, ".gitattributes")
	if err := os.WriteFile(attributes, []byte("notes/* filter=poison\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", ".gitattributes")
	repo.Git("commit", "-qm", "attributes")
	attack := filepath.Join(repo.Root, "attack")
	if err := os.MkdirAll(filepath.Join(attack, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		executable = filepath.ToSlash(executable)
	}
	command := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestHostileConfiguredProgram$ --"
	marker := filepath.Join(attack, "executed")
	hook := filepath.Join(attack, "hooks", "reference-transaction")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+command+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	assertTriggered := func(name string) {
		t.Helper()
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("hostile %s fixture did not run: %v", name, err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
	}
	runGitWithMarker := func(args ...string) {
		t.Helper()
		cmd := exec.Command(repo.GitPath, args...)
		cmd.Dir = repo.Root
		cmd.Env = append(append([]string(nil), repo.Env...), "GITLOCAL_HOSTILE_MARKER="+marker)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v\n%s", args, err, output)
		}
	}
	repo.Git("config", "filter.poison.clean", command)
	repo.Git("config", "filter.poison.required", "true")
	probe := filepath.Join(repo.Root, "notes", "probe.txt")
	if err := os.MkdirAll(filepath.Dir(probe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte("trigger filter\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGitWithMarker("add", "notes/probe.txt")
	assertTriggered("clean filter")
	repo.Git("reset", "-q", "--", "notes/probe.txt")
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	repo.Git("config", "core.hooksPath", filepath.Join(attack, "hooks"))
	runGitWithMarker("update-ref", "refs/gitlocal-hostile-probe", "HEAD")
	assertTriggered("reference-transaction hook")
	runGitWithMarker("update-ref", "-d", "refs/gitlocal-hostile-probe")
	assertTriggered("reference-transaction hook cleanup")
	repo.Git("config", "diff.external", command)
	app := filepath.Join(repo.Root, "app.txt")
	if err := os.WriteFile(app, []byte("changed for external diff\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGitWithMarker("diff", "--", "app.txt")
	assertTriggered("external diff")
	if err := os.WriteFile(app, []byte("application\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("config", "core.fsmonitor", command)
	runGitWithMarker("status", "--short")
	assertTriggered("fsmonitor")
	admin := filepath.Join(repo.Root, ".git")
	beforeIndex, err := os.ReadFile(filepath.Join(admin, "index"))
	if err != nil {
		t.Fatal(err)
	}
	beforeHead, err := os.ReadFile(filepath.Join(admin, "refs", "heads", "main"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := gitexec.New(append(repo.RunnerEnv(), "GITLOCAL_HOSTILE_MARKER="+marker))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := workspace.Init(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo.Root, "notes", "private.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Diff(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	result, err := history.Commit(ctx, r, repo.Root, "hostile config qualification")
	if err != nil || !result.Changed {
		t.Fatalf("snapshot failed: %+v %v", result, err)
	}
	content, err := history.Show(ctx, r, repo.Root, "", "latest", "notes/private.txt")
	if err != nil || !bytes.Equal(content, []byte("local only\n")) {
		t.Fatalf("snapshot changed raw bytes: %q %v", content, err)
	}
	if _, err := doctor.Run(ctx, r, repo.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("hostile Git command executed or stat failed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(admin, "index")); err != nil || !bytes.Equal(got, beforeIndex) {
		t.Fatalf("main index changed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(admin, "refs", "heads", "main")); err != nil || !bytes.Equal(got, beforeHead) {
		t.Fatalf("main HEAD changed: %v", err)
	}
}

// Git can invoke this test executable through configured commands on every
// platform. A marker makes those invocations observable without a shell binary.
func TestHostileConfiguredProgram(t *testing.T) {
	marker := os.Getenv("GITLOCAL_HOSTILE_MARKER")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("ran\n"), 0600); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
