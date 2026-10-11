package gitexec

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/testutil"
)

// httpsBackupRemote serves a bare repository over TLS through git
// http-backend and requires basic authentication.
func httpsBackupRemote(t *testing.T, repo *testutil.Repo) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "backup.git")
	for _, args := range [][]string{{"init", "--bare", "-q", remote}, {"--git-dir=" + remote, "config", "http.receivepack", "true"}} {
		cmd := exec.Command(repo.GitPath, args...)
		cmd.Env = repo.RunnerEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	execPath, err := exec.Command(repo.GitPath, "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=backup-user", "HOME=" + root}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "backup-user" || password != "backup-secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="backup"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return server.URL + "/backup.git", remote, ca
}

func TestBackupTransportUsesUserCredentialHelperForHTTPS(t *testing.T) {
	repo := testutil.New(t)
	url, remote, ca := httpsBackupRemote(t, repo)
	home := ""
	for _, entry := range repo.RunnerEnv() {
		if value, ok := strings.CutPrefix(entry, "HOME="); ok {
			home = value
		}
	}
	if home == "" || !strings.HasPrefix(home, repo.Root+string(filepath.Separator)) {
		t.Fatalf("test HOME isn't isolated: %q", home)
	}
	// A system helper (osxkeychain on macOS) is cleared first, so the test
	// never reads or stores credentials outside its HOME.
	writeConfig := func(helper string) {
		t.Helper()
		config := "[credential]\n\thelper =\n"
		if helper != "" {
			config += "[credential \"" + strings.TrimSuffix(url, "/backup.git") + "\"]\n\thelper = " + helper + "\n"
		}
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("")
	r, err := New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.backupConfig = []string{"http.sslCAInfo=" + ca}
	ctx := context.Background()
	store := filepath.Join(t.TempDir(), "stage.git")
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	blob, err := r.History(ctx, store, strings.NewReader("encrypted archive\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.History(ctx, store, strings.NewReader("100644 blob "+strings.TrimSpace(string(blob))+"\tarchive.enc\x00"), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := r.History(ctx, store, strings.NewReader("backup\n"), "commit-tree", strings.TrimSpace(string(tree)))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(commit))
	ref := "refs/gitlocal/backups/00000000-0000-4000-8000-000000000000/20260101T000000Z-00000000-0000-4000-8000-000000000001"
	// Without a helper Git can't authenticate, and it must not prompt.
	if err := r.BackupPush(ctx, store, url, id, ref); err == nil {
		t.Fatal("unauthenticated HTTPS push succeeded")
	}
	writeConfig(`"!f() { test \"$1\" = get && printf 'username=backup-user\\npassword=backup-secret\\n'; }; f"`)
	if err := r.BackupPush(ctx, store, url, id, ref); err != nil {
		t.Fatalf("HTTPS push through the URL's credential helper: %v", err)
	}
	cmd := exec.Command(repo.GitPath, "--git-dir="+remote, "rev-parse", ref)
	cmd.Env = repo.RunnerEnv()
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != id {
		t.Fatalf("pushed backup ref: %q %v", out, err)
	}
	fetched := filepath.Join(t.TempDir(), "fetch.git")
	if err := r.InitBare(ctx, fetched); err != nil {
		t.Fatal(err)
	}
	if err := r.BackupFetch(ctx, fetched, url, ref); err != nil {
		t.Fatalf("HTTPS fetch through the URL's credential helper: %v", err)
	}
	if out, err := r.History(ctx, fetched, nil, "rev-parse", "refs/heads/import"); err != nil || strings.TrimSpace(string(out)) != id {
		t.Fatalf("fetched backup: %q %v", out, err)
	}
}

func TestBackupArgsSeparateTheURL(t *testing.T) {
	for _, verb := range []string{"push", "fetch"} {
		args := backupArgs(verb, "-oProxyCommand=touch:x", "a:b")
		if args[0] != verb || len(args) < 4 || args[len(args)-3] != "--" || args[len(args)-2] != "-oProxyCommand=touch:x" || args[len(args)-1] != "a:b" {
			t.Fatalf("%s arguments: %q", verb, args)
		}
	}
}

// A backup host that redirects must not move the push to another host (which
// the backup destination checks never saw), with the credentials Git would
// fetch for that host.
func TestBackupTransportRefusesHTTPSRedirects(t *testing.T) {
	repo := testutil.New(t)
	url, remote, ca := httpsBackupRemote(t, repo)
	home := ""
	for _, entry := range repo.RunnerEnv() {
		if value, ok := strings.CutPrefix(entry, "HOME="); ok {
			home = value
		}
	}
	if home == "" || !strings.HasPrefix(home, repo.Root+string(filepath.Separator)) {
		t.Fatalf("test HOME isn't isolated: %q", home)
	}
	helper := `"!f() { test \"$1\" = get && printf 'username=backup-user\\npassword=backup-secret\\n'; }; f"`
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper =\n\thelper = "+helper+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target := strings.TrimSuffix(url, "/backup.git")
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(redirector.Close)
	r, err := New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Both servers use httptest's certificate.
	r.backupConfig = []string{"http.sslCAInfo=" + ca}
	ctx := context.Background()
	store := filepath.Join(t.TempDir(), "stage.git")
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	tree, err := r.History(ctx, store, strings.NewReader(""), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := r.History(ctx, store, strings.NewReader("backup\n"), "commit-tree", strings.TrimSpace(string(tree)))
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/gitlocal/backups/00000000-0000-4000-8000-000000000000/20260101T000000Z-00000000-0000-4000-8000-000000000001"
	if err := r.BackupPush(ctx, store, url, strings.TrimSpace(string(commit)), ref); err != nil {
		t.Fatalf("direct HTTPS push: %v", err)
	}
	redirected := "refs/gitlocal/backups/00000000-0000-4000-8000-000000000000/20260101T000000Z-00000000-0000-4000-8000-000000000002"
	if err := r.BackupPush(ctx, store, redirector.URL+"/backup.git", strings.TrimSpace(string(commit)), redirected); err == nil {
		t.Fatal("HTTPS push followed a redirect to another host")
	}
	cmd := exec.Command(repo.GitPath, "--git-dir="+remote, "rev-parse", "--verify", "--quiet", redirected)
	cmd.Env = repo.RunnerEnv()
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("redirected push landed on the other host: %s", out)
	}
}
