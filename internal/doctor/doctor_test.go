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
	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/keymaterial"
	"gitlocal/internal/overlay"
	"gitlocal/internal/restore"
	"gitlocal/internal/secureblob"
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

func TestDoctorReportsMissingAndAbsoluteOverlayInclude(t *testing.T) {
	repo, r, w := fixture(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte("port = 80\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "app.conf")
	repo.Git("commit", "-qm", "config")
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), filepath.Base(source))
	if err := os.WriteFile(executable, program, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "app.conf"), []byte("port = 8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := overlay.Add(ctx, r, repo.Root, "port", "app.conf", executable); err != nil {
		t.Fatal(err)
	}
	overlayIssue := func(want string) {
		t.Helper()
		report, err := Run(ctx, r, repo.Root)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range report.Findings {
			if finding.Check == "overlays" && finding.Severity == "error" && strings.Contains(finding.Message, want) {
				return
			}
		}
		t.Fatalf("doctor did not report %q: %+v", want, report.Findings)
	}
	configPath := filepath.Join(w.Common, "gitlocal", "overlay-filter.gitconfig")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	overlayIssue("no filter configuration file")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("config", "--local", "--replace-all", "include.path", configPath)
	report, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, finding := range report.Findings {
		if finding.Check == "overlays" && finding.Severity == "error" {
			t.Fatalf("own absolute include reported as an error: %+v", finding)
		}
		warned = warned || finding.Check == "overlays" && finding.Severity == "warning" && strings.Contains(finding.Message, "absolute path") && strings.Contains(finding.Action, "git local init")
	}
	if !warned {
		t.Fatalf("doctor did not warn about the absolute include: %+v", report.Findings)
	}
	repo.Git("config", "--local", "--replace-all", "include.path", filepath.Join(t.TempDir(), "moved", ".git", "gitlocal", "overlay-filter.gitconfig"))
	overlayIssue("no longer resolves")
}

func TestDoctorReportsSensitiveFileRuleNamingADirectory(t *testing.T) {
	repo, r, w := fixture(t)
	if err := os.MkdirAll(filepath.Join(repo.Root, "notes", "secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: "notes/secrets", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.Check == "encryption" && finding.Severity == "error" && strings.Contains(finding.Message, `"notes/secrets" is recorded as a file but is a directory`) {
			// The action names the correction command with the policy's key.
			if want := "git local encrypt migrate --key-file '" + policy.KeyFile + "' -- 'notes/secrets'"; !strings.Contains(finding.Action, want) {
				t.Fatalf("doctor action %q doesn't name %q", finding.Action, want)
			}
			return
		}
	}
	t.Fatalf("doctor did not report the kind mismatch: %+v", report.Findings)
}

func TestDoctorWarnsAboutManagedBlockWithoutStageRule(t *testing.T) {
	repo, r, w := fixture(t)
	path := filepath.Join(w.Common, "info", "exclude")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(data), exclude.StagePattern+"\n", "", 1)
	if legacy == string(data) {
		t.Fatalf("managed block lacks the stage rule:\n%s", data)
	}
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !has(report, "selection and ignores", "warning") || has(report, "exclusions", "error") {
		t.Fatalf("doctor did not warn about the outdated block: %+v", report.Findings)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, false, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, "notes"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), exclude.StagePattern+"\n") {
		t.Fatalf("selection change did not rewrite the block: %v\n%s", err, data)
	}
}

func TestDoctorReportsSensitiveRuleSpelledDifferently(t *testing.T) {
	repo, r, w := fixture(t)
	if err := os.MkdirAll(filepath.Join(repo.Root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes", "secret.txt"), []byte("s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	policy, err := secureblob.PolicyForKey(keyPath, w.Root, w.Common, []secureblob.PathRule{{Path: "notes/Secret.txt", Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	if err := secureblob.WriteLocked(common, policy, w.Root, w.Common); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.Check == "encryption" && finding.Severity == "error" && strings.Contains(finding.Message, `"notes/Secret.txt" is spelled "notes/secret.txt" on disk`) {
			return
		}
	}
	t.Fatalf("doctor did not report the spelling mismatch: %+v", report.Findings)
}

func TestDoctorReportsStageFileInIndex(t *testing.T) {
	repo, r, _ := fixture(t)
	name := ".gitlocal-tmp-0b8c3d4e-1111-4222-8333-444455556666"
	if err := os.WriteFile(filepath.Join(repo.Root, name), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.Git("add", "-f", "--", name)
	report, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.Check == "staging files" && strings.Contains(finding.Message, name+" (staged in the index)") {
			return
		}
	}
	t.Fatalf("doctor did not report the staged stage file: %+v", report.Findings)
}

func TestDoctorGivesNextCommandForEveryPendingJournal(t *testing.T) {
	repo, r, w := fixture(t)
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	ids := map[string]string{}
	for _, kind := range []string{"config", "restore", "encrypt"} {
		id, err := storage.UUID()
		if err != nil {
			t.Fatal(err)
		}
		ids[kind] = id
		body := `{"direction":"apply"}`
		if kind == "config" {
			body = `{"direction":"rollback"}`
		}
		if err := meta.AtomicWrite("transactions/"+kind+"-"+id+".json", []byte(body+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The encryption journal blocks every session; doctor still explains each.
	result, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		ids["config"]:  {"git local recover " + ids["config"] + " --rollback.", "git local recover " + ids["config"] + " --abandon"},
		ids["restore"]: {"git local recover " + ids["restore"] + " --resume or --rollback", "git local recover " + ids["restore"] + " --abandon"},
		ids["encrypt"]: {"git local encrypt recover " + ids["encrypt"] + " --resume or --rollback", "git local recover " + ids["encrypt"] + " --abandon"},
	}
	for id, fragments := range want {
		found := false
		for _, f := range result.Findings {
			if f.Check != "transaction "+id || f.Severity != "error" {
				continue
			}
			found = true
			for _, fragment := range fragments {
				if !strings.Contains(f.Action, fragment) {
					t.Fatalf("action for %s lacks %q: %s", id, fragment, f.Action)
				}
			}
		}
		if !found {
			t.Fatalf("no finding for %s:\n%s", id, result.Human())
		}
	}
}

// GL-119: doctor names a completed seed whose file list is lost, with the
// command that clears it, and a seed file list no journal names, with the
// command that deletes it.
func TestDoctorReportsLostAndOrphanSeedLists(t *testing.T) {
	repo, r, w := fixture(t)
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	const lost, orphan = "0b9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69", "1c9e8a1c-7d3f-4c2a-9e5b-1f2d3c4b5a69"
	for _, dir := range []string{"transactions/completed", "transactions/seed-files"} {
		if err := meta.MkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	journal := `{"schemaVersion": 1, "kind": "seed", "id": "` + lost + `", "files": [], "filesDigest": "` + strings.Repeat("a", 64) + `"}` + "\n"
	if err := meta.AtomicWrite("transactions/completed/seed-"+lost+".json", []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	if err := meta.AtomicWrite("transactions/seed-files/"+orphan+".json", []byte("{\"files\": []}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	human := result.Human()
	if !has(result, "journal "+lost, "error") || !strings.Contains(human, "git local recover "+lost+" --abandon") {
		t.Fatalf("lost seed list not reported with its remedy:\n%s", human)
	}
	if !has(result, "seed file lists", "warning") || !strings.Contains(human, "rm '"+filepath.Join(w.Common, "gitlocal", "transactions", "seed-files", orphan+".json")+"'") {
		t.Fatalf("orphan seed list not reported with its command:\n%s", human)
	}
}

// GL-124: while a transaction is pending, the guards finding names the
// command that resolves it (the transaction's own next step), not "Inspect
// selection metadata and retry".
func TestGuardsFindingNamesThePendingTransactionsNextStep(t *testing.T) {
	t.Parallel()
	repo, r, w := fixture(t)
	id := "00000000-0000-4000-8000-000000000001"
	if err := os.WriteFile(filepath.Join(w.Common, "gitlocal", "transactions", "restore-"+id+".json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, f := range report.Findings {
		actions[f.Check] = f.Action
	}
	if actions["guards"] == "" || actions["guards"] != actions["transaction "+id] || !strings.Contains(actions["guards"], "recover "+id) {
		t.Fatalf("guards action %q, transaction action %q", actions["guards"], actions["transaction "+id])
	}
}

// GL-131: worktrees whose core.ignorecase settings differ share one managed
// ignore block; doctor warns, naming each worktree and its value, and is
// silent while they agree.
func TestDoctorWarnsWhenWorktreesDisagreeOnIgnoreCase(t *testing.T) {
	repo, r, _ := fixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "aux", linked)
	if _, err := workspace.Init(ctx, r, linked); err != nil {
		t.Fatal(err)
	}
	repo.Git("config", "extensions.worktreeConfig", "true")
	repo.Git("config", "core.ignorecase", "false")
	finding := func() *Finding {
		t.Helper()
		result, err := Run(ctx, r, repo.Root)
		if err != nil {
			t.Fatal(err)
		}
		for i, f := range result.Findings {
			if f.Check == "core.ignorecase" {
				return &result.Findings[i]
			}
		}
		return nil
	}
	if f := finding(); f != nil {
		t.Fatalf("doctor reports agreeing worktrees: %+v", *f)
	}
	repo.At(linked, "config", "--worktree", "core.ignorecase", "true")
	f := finding()
	if f == nil || f.Severity != "warning" || !strings.Contains(f.Message, repo.Root+": false") || !strings.Contains(f.Message, linked+": true") {
		t.Fatalf("doctor doesn't warn about disagreeing worktrees: %+v", f)
	}
	repo.At(linked, "config", "--worktree", "core.ignorecase", "false")
	if f := finding(); f != nil {
		t.Fatalf("doctor reports agreeing worktrees: %+v", *f)
	}
}

// GL-131 review: a worktree whose core.ignorecase can't be read is a warning
// naming it, the other worktrees are still compared, and doctor's exit status
// is unaffected. "1k" is a value Git accepts (as true) but git-local doesn't
// parse.
func TestDoctorIgnoreCaseReadFailureNamesTheWorktree(t *testing.T) {
	repo, r, _ := fixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	odd := filepath.Join(t.TempDir(), "odd")
	repo.Git("worktree", "add", "-q", "-b", "aux", linked)
	repo.Git("worktree", "add", "-q", "-b", "odd", odd)
	for _, root := range []string{linked, odd} {
		if _, err := workspace.Init(ctx, r, root); err != nil {
			t.Fatal(err)
		}
	}
	repo.Git("config", "extensions.worktreeConfig", "true")
	repo.Git("config", "core.ignorecase", "false")
	repo.At(linked, "config", "--worktree", "core.ignorecase", "true")
	repo.At(odd, "config", "--worktree", "core.ignorecase", "1k")
	result, err := Run(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	var unreadable, mismatch bool
	for _, f := range result.Findings {
		if f.Severity == "error" {
			t.Fatalf("doctor reports an error: %+v", f)
		}
		if f.Check != "core.ignorecase" {
			continue
		}
		switch {
		case strings.Contains(f.Message, "could not read") && strings.Contains(f.Message, odd):
			unreadable = f.Severity == "warning"
		case strings.Contains(f.Message, repo.Root+": false") && strings.Contains(f.Message, linked+": true") && !strings.Contains(f.Message, odd):
			mismatch = f.Severity == "warning" && strings.Contains(f.Action, "--unset core.ignorecase") && strings.Contains(f.Action, "case-insensitive volumes")
		default:
			t.Fatalf("unexpected core.ignorecase finding: %+v", f)
		}
	}
	if !unreadable || !mismatch {
		t.Fatalf("unreadable warning %t, mismatch warning %t: %+v", unreadable, mismatch, result.Findings)
	}
}
