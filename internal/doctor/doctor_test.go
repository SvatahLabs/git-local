package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/restore"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

var ctx = context.Background()

func fixture(t *testing.T) (*testutil.Repo, *gitexec.Runner, *workspace.Context) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo.Root, "notes/a"), []byte("TOP_SECRET_opaque_bytes\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = configtx.Change(ctx, r, repo.Root, true, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err = history.Commit(ctx, r, repo.Root, "first"); err != nil {
		t.Fatal(err)
	}
	return repo, r, w
}
func durable(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		fi, e := d.Info()
		if e != nil {
			return e
		}
		var data []byte
		if fi.Mode()&os.ModeSymlink != 0 {
			link, e := os.Readlink(p)
			if e != nil {
				return e
			}
			data = []byte(link)
		} else if !d.IsDir() {
			data, e = os.ReadFile(p)
			if e != nil {
				return e
			}
		}
		out[p] = fmt.Sprintf("%v:%d:%s", fi.Mode(), fi.ModTime().UnixNano(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func has(r Report, check, severity string) bool {
	for _, f := range r.Findings {
		if f.Check == check && f.Severity == severity {
			return true
		}
	}
	return false
}
func TestHealthyDoctorIsReadOnly(t *testing.T) {
	repo, r, w := fixture(t)
	before := durable(t, repo.Root)
	result, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if result.HasIssues() || result.WorkspaceID != w.WorkspaceID || result.RepositoryID != w.RepositoryID {
		t.Fatal(result.Human())
	}
	for _, name := range []string{"git", "configuration", "workspaces", "transactions", "journals", "exclusions", "selection and ignores", "main ownership", "objects", "history"} {
		if !has(result, name, "ok") {
			t.Fatal("missing healthy check", name, result.Human())
		}
	}
	b, err := result.JSON()
	if err != nil || strings.Contains(string(b), "TOP_SECRET_opaque_bytes") {
		t.Fatal("content disclosed", err)
	}
	if !reflect.DeepEqual(before, durable(t, repo.Root)) {
		t.Fatal("doctor changed repository durable state")
	}
}
func TestDoctorReportsPendingAndCorruptJournal(t *testing.T) {
	repo, r, w := fixture(t)
	id, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	if err = meta.AtomicWrite("transactions/config-"+id+".json", []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restoreID, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	if err = meta.AtomicWrite("transactions/restore-"+restoreID+".json", []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	meta.Close()
	before := durable(t, repo.Root)
	result, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !has(result, "transaction "+id, "error") || !has(result, "journal "+id, "error") ||
		!has(result, "transaction "+restoreID, "error") || !has(result, "journal "+restoreID, "error") {
		t.Fatal(result.Human())
	}
	if has(result, "journals", "ok") {
		t.Fatal("corrupt journals reported healthy", result.Human())
	}
	if !strings.Contains(result.Human(), "git local recover "+id) {
		t.Fatal("no actionable recovery ID", result.Human())
	}
	if !reflect.DeepEqual(before, durable(t, repo.Root)) {
		t.Fatal("doctor touched pending transaction")
	}
}

func TestDoctorAcceptsCompletedRestoreFromOtherAndRetiredWorkspace(t *testing.T) {
	repo, r, main := fixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "aux", linked)
	other, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(linked, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(linked, "notes/a"), []byte("linked snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = configtx.Change(ctx, r, linked, true, "notes/a"); err != nil {
		t.Fatal(err)
	}
	if _, err = history.Commit(ctx, r, linked, "linked"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(linked, "notes/a"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := restore.Execute(ctx, r, linked, "", "latest", []string{"notes/a"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(linked, "notes/a"), []byte("changed again"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = restore.Execute(ctx, r, linked, main.WorkspaceID, "latest", []string{"notes/a"}, true); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{linked, repo.Root} {
		report, e := Run(ctx, r, root)
		if e != nil || report.HasIssues() {
			t.Fatal(root, report.Human(), e)
		}
	}
	journal := filepath.Join(main.Common, "gitlocal", "transactions", "completed", "restore-"+first.TransactionID+".json")
	original, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var pendingData map[string]any
	if err = json.Unmarshal(original, &pendingData); err != nil {
		t.Fatal(err)
	}
	pendingData["outcome"] = ""
	pendingJSON, err := json.Marshal(pendingData)
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(main.Common, "gitlocal", "transactions", "restore-"+first.TransactionID+".json")
	if err = os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pendingPath, pendingJSON, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, r, repo.Root)
	if err != nil || !has(report, "transaction "+first.TransactionID, "error") || has(report, "journal "+first.TransactionID, "error") {
		t.Fatal("remote pending journal was misdiagnosed", report.Human(), err)
	}
	if err = os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(journal, original, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("worktree", "remove", "--force", linked)
	report, err = Run(ctx, r, repo.Root)
	if err != nil || report.HasIssues() {
		t.Fatal(report.Human(), err)
	}
	missing := false
	for _, finding := range report.Findings {
		if finding.Code == "workspace-missing" && finding.WorkspaceID == other.WorkspaceID && finding.Severity == "warning" {
			missing = true
		}
	}
	jsonReport, err := report.JSON()
	if !missing || err != nil || !strings.Contains(string(jsonReport), `"code": "workspace-missing"`) || !strings.Contains(string(jsonReport), `"workspaceId": "`+other.WorkspaceID+`"`) {
		t.Fatal("missing structured worktree finding", report.Human(), err)
	}
	if _, err = configtx.Retire(ctx, r, repo.Root, other.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, r, repo.Root)
	if err != nil || report.HasIssues() {
		t.Fatal(report.Human(), err)
	}
	if err = os.WriteFile(journal, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, r, repo.Root)
	if err != nil || !has(report, "journal "+first.TransactionID, "error") {
		t.Fatal(report.Human(), err)
	}
}
func TestDoctorFindsUnsupportedConfigurationAndOwnership(t *testing.T) {
	for _, kind := range []string{"store config", "selection schema", "main ownership", "ignore override", "missing blob"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			repo, r, w := fixture(t)
			switch kind {
			case "store config":
				if err := os.WriteFile(filepath.Join(w.Store, "config"), []byte("[core]\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "selection schema":
				p := filepath.Join(w.Common, "gitlocal/workspaces", w.WorkspaceID, "selection.json")
				b := []byte(`{"schemaVersion":999,"generation":0,"include":[],"exclude":[]}`)
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "main ownership":
				repo.Git("add", "-f", "notes/a")
			case "ignore override":
				if err := os.WriteFile(filepath.Join(repo.Root, ".gitignore"), []byte("!notes/\n!notes/a\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing blob":
				snap, e := history.Resolve(ctx, r, repo.Root, "", "latest")
				if e != nil {
					t.Fatal(e)
				}
				id := snap.Files["notes/a"].ID
				if err := os.Remove(filepath.Join(w.Store, "objects", id[:2], id[2:])); err != nil {
					t.Fatal(err)
				}
			}
			before := durable(t, repo.Root)
			result, err := Run(ctx, r, repo.Root)
			if err != nil {
				t.Fatal(err)
			}
			want := "configuration"
			if kind == "main ownership" {
				want = "main ownership"
			}
			if kind == "ignore override" {
				want = "selection and ignores"
			}
			if kind == "missing blob" {
				want = "objects"
			}
			if !has(result, want, "error") {
				t.Fatal("missed", kind, result.Human())
			}
			if strings.Contains(result.Human(), "TOP_SECRET_opaque_bytes") {
				t.Fatal("content leaked")
			}
			if !reflect.DeepEqual(before, durable(t, repo.Root)) {
				t.Fatal("doctor repaired", kind)
			}
		})
	}
}
func TestAuditChecksOlderSnapshots(t *testing.T) {
	repo, r, w := fixture(t)
	first, err := history.Resolve(ctx, r, repo.Root, "", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo.Root, "notes/a"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = history.Commit(ctx, r, repo.Root, "second"); err != nil {
		t.Fatal(err)
	}
	count, err := history.AuditWorkspaceLocked(ctx, r, *w, w.WorkspaceID)
	if err != nil || count != 2 {
		t.Fatal(count, err)
	}
	id := first.Files["notes/a"].ID
	if err = os.Remove(filepath.Join(w.Store, "objects", id[:2], id[2:])); err != nil {
		t.Fatal(err)
	}
	if _, err = history.AuditWorkspaceLocked(ctx, r, *w, w.WorkspaceID); err == nil {
		t.Fatal("older snapshot corruption missed")
	}
}
