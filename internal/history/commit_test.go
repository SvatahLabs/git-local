package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func fixture(t *testing.T) (*testutil.Repo, *gitexec.Runner, *workspace.Context) {
	t.Helper()
	r := testutil.New(t)
	g, err := gitexec.New(r.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	w, err := workspace.Init(context.Background(), g, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	return r, g, w
}
func write(t *testing.T, root, path string, b []byte) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func change(t *testing.T, g *gitexec.Runner, root string, track bool, paths ...string) {
	t.Helper()
	if _, err := configtx.Change(context.Background(), g, root, track, paths...); err != nil {
		t.Fatal(err)
	}
}
func save(t *testing.T, g *gitexec.Runner, root string) Result {
	t.Helper()
	result, err := Commit(context.Background(), g, root, "private snapshot\n\nraw message")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("changed=%v, Git subprocesses=%d", result.Changed, result.Subprocesses)
	return result
}
func gitHistory(t *testing.T, g *gitexec.Runner, w *workspace.Context, args ...string) []byte {
	t.Helper()
	out, err := g.History(context.Background(), w.Store, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func mainState(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, part := range []string{"index", "HEAD", "refs", "objects", "logs"} {
		base := filepath.Join(root, ".git", part)
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !d.IsDir() {
				b, e := os.ReadFile(p)
				if e != nil {
					return e
				}
				result[p] = string(b)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}
func TestRawSnapshotsAndMainIsolation(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	// Project attributes and filters must never transform local content.
	write(t, r.Root, ".gitattributes", []byte("* text eol=lf filter=poison\n"))
	r.Git("config", "filter.poison.clean", "exit 97")
	r.Git("config", "filter.poison.required", "true")
	raw := []byte("private-unique\x00\xff\r\nlast\r\n")
	write(t, r.Root, "notes/data", raw)
	write(t, r.Root, "notes/empty", nil)
	write(t, r.Root, "notes/run", []byte("#!/bin/sh\r\n"))
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(r.Root, "notes/run"), 0755)
	}
	link := os.Symlink("../../outside-not-read", filepath.Join(r.Root, "notes/link")) == nil
	change(t, g, r.Root, true, "notes")
	before := mainState(t, r.Root)
	first := save(t, g, r.Root)
	if !first.Changed {
		t.Fatal("first capture was no-op")
	}
	if got := gitHistory(t, g, w, "cat-file", "blob", first.ID+":files/notes/data"); !bytes.Equal(got, raw) {
		t.Fatal("raw bytes changed")
	}
	if got := gitHistory(t, g, w, "cat-file", "blob", first.ID+":files/notes/empty"); len(got) != 0 {
		t.Fatal("empty blob changed")
	}
	if link {
		if got := gitHistory(t, g, w, "cat-file", "blob", first.ID+":files/notes/link"); string(got) != "../../outside-not-read" {
			t.Fatal("link dereferenced")
		}
	}
	entries := string(gitHistory(t, g, w, "ls-tree", "-r", first.ID+":files"))
	if runtime.GOOS != "windows" && !strings.Contains(entries, "100755 blob") {
		t.Fatal("executable mode lost")
	}
	if link && !strings.Contains(entries, "120000 blob") {
		t.Fatal("link mode lost")
	}
	_, metadata, err := readPrevious(context.Background(), g, w.Store, first.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil || metadata.MainHead == "" {
		t.Fatal(metadata, err)
	}
	second := save(t, g, r.Root)
	if second.Changed || second.ID != first.ID {
		t.Fatal("identical capture made a commit")
	}
	if !reflect.DeepEqual(before, mainState(t, r.Root)) {
		t.Fatal("main Git state changed")
	}
	if got, _ := os.ReadFile(filepath.Join(r.Root, "notes/data")); !bytes.Equal(got, raw) {
		t.Fatal("working bytes changed")
	}
	gitHistory(t, g, w, "fsck", "--full", "--no-reflogs")
}

func TestBatchedRawCaptureKeepsFiltersAndFaultsOutOfMainline(t *testing.T) {
	repo, g, w := fixture(t)
	repo.Commit()
	write(t, repo.Root, ".gitattributes", []byte("notes/* filter=poison\n"))
	repo.Git("config", "filter.poison.clean", "exit 97")
	repo.Git("config", "filter.poison.required", "true")
	for i := 0; i < 40; i++ {
		path := filepath.Join("notes", fmt.Sprintf("%02d.bin", i))
		write(t, repo.Root, path, []byte{byte(i), 0, 0xff, '\n'})
	}
	change(t, g, repo.Root, true, "notes")
	before := mainState(t, repo.Root)
	count := 0
	_, err := commit(context.Background(), g, repo.Root, "interrupted batch", func(at string) error {
		if at == "file" {
			count++
			if count == 5 {
				return errors.New("simulated interruption")
			}
		}
		return nil
	})
	if err == nil || count != 5 {
		t.Fatalf("batch fault was not observed: count=%d err=%v", count, err)
	}
	if log, err := Log(context.Background(), g, repo.Root, 10); err != nil || len(log) != 0 {
		t.Fatalf("fault published a snapshot: %+v %v", log, err)
	}
	first := save(t, g, repo.Root)
	if !first.Changed {
		t.Fatal("batch snapshot was a no-op")
	}
	if first.Subprocesses >= 120 {
		t.Fatalf("batch snapshot launched %d Git processes for 40 files", first.Subprocesses)
	}
	for _, i := range []int{0, 17, 39} {
		path := filepath.ToSlash(filepath.Join("notes", fmt.Sprintf("%02d.bin", i)))
		want := []byte{byte(i), 0, 0xff, '\n'}
		if got := gitHistory(t, g, w, "cat-file", "blob", first.ID+":files/"+path); !bytes.Equal(got, want) {
			t.Fatalf("batched blob %q was transformed: %q", path, got)
		}
	}
	second := save(t, g, repo.Root)
	if second.Changed || second.ID != first.ID {
		t.Fatal("unchanged batch created another snapshot")
	}
	if !reflect.DeepEqual(before, mainState(t, repo.Root)) {
		t.Fatal("batched capture changed mainline state")
	}
}
func TestNoChangeRulesDeletionsAndUnborn(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, "notes/data", []byte("local"))
	os.Mkdir(filepath.Join(r.Root, "notes/empty"), 0700)
	change(t, g, r.Root, true, "notes")
	first := save(t, g, r.Root)
	_, m, err := readPrevious(context.Background(), g, w.Store, first.ID, w.RepositoryID, w.WorkspaceID)
	if err != nil || m.MainHead != "" {
		t.Fatal(m, err)
	}
	commit := string(gitHistory(t, g, w, "cat-file", "commit", first.ID))
	if strings.Contains(commit, "\nparent ") {
		t.Fatal("first snapshot has parent")
	}
	change(t, g, r.Root, false, "notes")
	change(t, g, r.Root, true, "notes")
	if next := save(t, g, r.Root); next.Changed {
		t.Fatal("generation alone made a snapshot")
	}
	change(t, g, r.Root, false, "notes/absent")
	excluded := save(t, g, r.Root)
	if !excluded.Changed {
		t.Fatal("exclusion-only change lost")
	}
	os.Remove(filepath.Join(r.Root, "notes/data"))
	deleted := save(t, g, r.Root)
	if !deleted.Changed {
		t.Fatal("deletion not recorded")
	}
	if got := gitHistory(t, g, w, "ls-tree", "-r", deleted.ID+":files"); len(got) != 0 {
		t.Fatal("empty dirs or deleted content retained")
	}
	change(t, g, r.Root, false, "notes")
	empty := save(t, g, r.Root)
	if !empty.Changed {
		t.Fatal("deselection metadata lost")
	}
	if save(t, g, r.Root).Changed {
		t.Fatal("empty snapshot no-op failed")
	}
	if got := gitHistory(t, g, w, "rev-list", "--count", Ref(w.WorkspaceID)); string(got) != "4\n" {
		t.Fatal("wrong snapshot count", string(got))
	}
}
func TestPublicationInterruption(t *testing.T) {
	for _, stage := range []string{"validated", "file", "files-tree", "commit-object", "before-publish", "published"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			r, g, w := fixture(t)
			write(t, r.Root, "local", []byte("old"))
			change(t, g, r.Root, true, "local")
			old := save(t, g, r.Root)
			write(t, r.Root, "local", []byte("new"))
			result, err := commit(context.Background(), g, r.Root, "new", func(at string) error {
				if at == stage {
					return errors.New("interrupted")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault missed")
			}
			now, e := head(context.Background(), g, w.Store, Ref(w.WorkspaceID))
			if e != nil {
				t.Fatal(e)
			}
			if stage == "published" {
				if now != result.ID || !result.Changed {
					t.Fatal("published commit lost")
				}
				if save(t, g, r.Root).Changed {
					t.Fatal("retry duplicated published snapshot")
				}
			} else {
				if now != old.ID {
					t.Fatal("unpublished snapshot became visible")
				}
				if !save(t, g, r.Root).Changed {
					t.Fatal("retry failed")
				}
			}
			gitHistory(t, g, w, "fsck", "--full", "--no-reflogs")
		})
	}
}

func TestSnapshotProcessDeathBeforePublish(t *testing.T) {
	if root := os.Getenv("GITLOCAL_HISTORY_CRASH_ROOT"); root != "" {
		r, err := gitexec.New(os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		_, err = commit(context.Background(), r, root, "interrupted process", func(at string) error {
			if at == "before-publish" {
				if err := os.WriteFile(os.Getenv("GITLOCAL_HISTORY_CRASH_MARKER"), []byte("ready"), 0600); err != nil {
					return err
				}
				for {
					time.Sleep(time.Hour)
				}
			}
			return nil
		})
		t.Fatalf("child returned before process death: %v", err)
	}
	repo, r, w := fixture(t)
	repo.Commit()
	write(t, repo.Root, "local", []byte("old"))
	change(t, r, repo.Root, true, "local")
	old := save(t, r, repo.Root)
	write(t, repo.Root, "local", []byte("new"))
	before := mainState(t, repo.Root)
	marker := filepath.Join(t.TempDir(), "ready")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestSnapshotProcessDeathBeforePublish$")
	child.Env = append(repo.RunnerEnv(), "GITLOCAL_HISTORY_CRASH_ROOT="+repo.Root, "GITLOCAL_HISTORY_CRASH_MARKER="+marker)
	var output bytes.Buffer
	child.Stdout = &output
	child.Stderr = &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			ready = true
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if !ready {
		t.Fatalf("child did not reach publication boundary: %s", output.String())
	}
	if log, err := Log(context.Background(), r, repo.Root, 10); err != nil || len(log) != 1 || log[0].ID != old.ID {
		t.Fatalf("interrupted process published history: %+v %v", log, err)
	}
	if got := save(t, r, repo.Root); !got.Changed || got.ID == old.ID {
		t.Fatalf("retry after process death failed: %+v", got)
	}
	if !reflect.DeepEqual(before, mainState(t, repo.Root)) {
		t.Fatal("process interruption or retry changed mainline state")
	}
	gitHistory(t, r, w, "fsck", "--full", "--no-reflogs")
}
func TestCaptureConflictsFailBeforePublication(t *testing.T) {
	for _, which := range []string{"bytes", "added", "mode", "selection", "ignore", "ownership", "ref"} {
		t.Run(which, func(t *testing.T) {
			t.Parallel()
			r, g, w := fixture(t)
			write(t, r.Root, "notes/local", []byte("old"))
			change(t, g, r.Root, true, "notes")
			previous := save(t, g, r.Root)
			write(t, r.Root, "notes/local", []byte("new"))
			_, err := commit(context.Background(), g, r.Root, "conflicting", func(at string) error {
				if at != "before-publish" {
					return nil
				}
				switch which {
				case "bytes":
					write(t, r.Root, "notes/local", []byte("changed during capture"))
				case "added":
					write(t, r.Root, "notes/added", []byte("new"))
				case "mode":
					if runtime.GOOS == "windows" {
						write(t, r.Root, "notes/local", []byte("mode surrogate changed"))
					} else {
						os.Chmod(filepath.Join(r.Root, "notes/local"), 0755)
					}
				case "selection":
					p := filepath.Join(w.Common, "gitlocal/workspaces", w.WorkspaceID, "selection.json")
					b, _ := os.ReadFile(p)
					os.WriteFile(p, append(b, ' '), 0600)
				case "ignore":
					f, e := os.OpenFile(w.Exclude, os.O_APPEND|os.O_WRONLY, 0600)
					if e != nil {
						t.Fatal(e)
					}
					f.WriteString("# edit\n")
					f.Close()
				case "ownership":
					r.Git("add", "-f", "notes/local")
				case "ref":
					gitHistory(t, g, w, "update-ref", "-d", Ref(w.WorkspaceID), previous.ID)
				}
				return nil
			})
			if err == nil {
				t.Fatal("capture conflict ignored")
			}
			now, e := head(context.Background(), g, w.Store, Ref(w.WorkspaceID))
			if e != nil {
				t.Fatal(e)
			}
			if which == "ref" {
				if now != "" {
					t.Fatal("stale update recreated ref")
				}
			} else if now != previous.ID {
				t.Fatal("conflict changed history")
			}
		})
	}
}
func TestNormalAndMirrorPushIsolation(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	write(t, r.Root, "local", []byte("unique-only-in-independent-store\x00\xff"))
	change(t, g, r.Root, true, "local")
	snapshot := save(t, g, r.Root)
	blob := strings.TrimSpace(string(gitHistory(t, g, w, "rev-parse", snapshot.ID+":files/local")))
	for _, mirror := range []bool{false, true} {
		remote := filepath.Join(t.TempDir(), "remote.git")
		r.Git("init", "--bare", "--template=", remote)
		args := []string{"-c", "protocol.file.allow=always", "push"}
		if mirror {
			args = append(args, "--mirror", remote)
		} else {
			args = append(args, remote, "refs/heads/main")
		}
		r.Git(args...)
		if refs := r.At(remote, "for-each-ref", "--format=%(refname)"); strings.Contains(string(refs), "gitlocal") {
			t.Fatal("local ref leaked")
		}
		objects := r.At(remote, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
		if bytes.Contains(objects, []byte(blob)) || bytes.Contains(objects, []byte(snapshot.ID)) {
			t.Fatal("private object leaked through push")
		}
	}
}
func TestPendingConfigurationBlocksCommit(t *testing.T) {
	r, g, w := fixture(t)
	write(t, r.Root, "local", []byte("private"))
	change(t, g, r.Root, true, "local")
	id, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	write(t, w.Common, "gitlocal/transactions/config-"+id+".json", []byte("{}"))
	if _, err = Commit(context.Background(), g, r.Root, "blocked"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("pending transaction ignored", err)
	}
}

func TestWorkspaceHeadsAndMainContextChanges(t *testing.T) {
	r, g, w := fixture(t)
	r.Commit()
	write(t, r.Root, "local", []byte("first workspace"))
	change(t, g, r.Root, true, "local")
	first := save(t, g, r.Root)
	r.Git("checkout", "-qb", "other")
	if result := save(t, g, r.Root); result.Changed || result.ID != first.ID {
		t.Fatal("branch context alone made a snapshot")
	}
	linked := filepath.Join(t.TempDir(), "linked")
	r.Git("worktree", "add", "-qb", "linked", linked)
	other, err := workspace.Init(context.Background(), g, linked)
	if err != nil {
		t.Fatal(err)
	}
	write(t, linked, "local", []byte("second workspace"))
	change(t, g, linked, true, "local")
	second := save(t, g, linked)
	if other.WorkspaceID == w.WorkspaceID || second.ID == first.ID {
		t.Fatal("workspace histories conflated")
	}
	current, err := head(context.Background(), g, w.Store, Ref(w.WorkspaceID))
	if err != nil || current != first.ID {
		t.Fatal("other workspace advanced current history", err)
	}
	if bytes.Contains(gitHistory(t, g, w, "cat-file", "commit", second.ID), []byte("\nparent ")) {
		t.Fatal("cross-workspace parent")
	}
}
