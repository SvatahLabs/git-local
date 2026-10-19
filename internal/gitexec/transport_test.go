package gitexec

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// GL-113: a failed backup push names its cause from Git's messages, and never
// prints the credentials of a URL.
func TestBackupTransportFailuresNameTheirCause(t *testing.T) {
	// A port nothing listens on.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	listener.Close()
	for _, tc := range []struct{ name, cause string }{
		{"certificate", "TLS certificate"},
		{"authentication", "authentication failed"},
		{"proxy", "couldn't be reached"},
		{"connection", "couldn't be reached"},
		{"refusal", "refused the request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
			config := "[credential]\n\thelper =\n\thelper = " + helper + "\n"
			settings := []string{"http.sslCAInfo=" + ca}
			switch tc.name {
			case "certificate":
				settings = nil
				url = strings.Replace(url, "https://", "https://backup-user:url-secret@", 1)
			case "authentication":
				config = "[credential]\n\thelper =\n"
			case "proxy":
				settings = append(settings, "http.proxy=http://proxy-user:proxy-secret@"+closed)
			case "connection":
				url = "https://backup-user:url-secret@" + closed + "/backup.git"
			case "refusal":
				hook := filepath.Join(remote, "hooks", "pre-receive")
				if err := os.WriteFile(hook, []byte("#!/bin/sh\necho no backups here >&2\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			r, err := New(repo.RunnerEnv())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.backupConfig = settings
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
			err = r.BackupPush(ctx, store, url, strings.TrimSpace(string(commit)), ref)
			var transport *TransportError
			if !errors.As(err, &transport) || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("push failure doesn't name %q: %v", tc.cause, err)
			}
			for _, secret := range []string{"url-secret", "proxy-secret", "backup-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("push failure prints a credential: %v", err)
				}
			}
			var ge *Error
			if !errors.As(err, &ge) || ge.Command != "push" {
				t.Fatalf("transport failure lost its Git error: %v", err)
			}
			cmd := exec.Command(repo.GitPath, "--git-dir="+remote, "rev-parse", "--verify", "--quiet", ref)
			cmd.Env = repo.RunnerEnv()
			if out, err := cmd.Output(); err == nil {
				t.Fatalf("failed push landed: %s", out)
			}
		})
	}
}

func TestRedactRemovesURLCredentials(t *testing.T) {
	got := redact("fatal: unable to access 'https://user:pass@example.com/r.git/': x; proxy http://p:q@proxy:8080\x1b[0m")
	if strings.Contains(got, "pass") || strings.Contains(got, "p:q") || strings.Contains(got, "\x1b") || !strings.Contains(got, "https://***@example.com/r.git/") {
		t.Fatalf("redacted: %q", got)
	}
}

// A password may hold a quote, and a remote's message may carry C1 controls
// (U+009B starts an escape sequence on some terminals) or bidirectional
// overrides; none of them reaches the terminal.
func TestRedactRemovesQuotedPasswordsAndC1Controls(t *testing.T) {
	got := redact("fatal: unable to access 'https://user:pa'ss\"w@example.com/r.git/': remote: \u009b31mred\u202eevil")
	if strings.Contains(got, "pa'ss") || strings.Contains(got, "ss\"w") || strings.ContainsAny(got, "\u009b\u202e") || !strings.Contains(got, "https://***@example.com/r.git/") || !strings.Contains(got, "31mred") {
		t.Fatalf("redacted: %q", got)
	}
}

// Passwords Git couldn't parse (a slash in them) and the destination ssh
// prints without a scheme are masked too; a cause is read from Git's words,
// not from a URL or host that happens to contain one; and a missing backup
// ref says so instead of blaming permissions.
func TestTransportCausesIgnoreURLsAndMaskEveryPasswordForm(t *testing.T) {
	for _, tc := range []struct{ stderr, cause string }{
		{"fatal: Invalid proxy URL 'http://bob:ab/cd+ef@proxy:8080': only SOCKS proxies support paths", "couldn't be reached"},
		{"backup:pw/cd@backup.example: Permission denied (publickey).\nfatal: Could not read from remote repository.", "authentication failed"},
		{"To https://gitproxy.example.com/backup.git\n ! [remote rejected] refs/gitlocal/backups/x -> refs/gitlocal/backups/x (pre-receive hook declined)\nerror: failed to push some refs to 'https://gitproxy.example.com/backup.git'", "refused the request"},
		{"fatal: couldn't find remote ref refs/gitlocal/backups/00000000-0000-4000-8000-000000000000/x", "isn't on the remote"},
		{"fatal: unable to access 'https://u:ab@cd+ef@backup.example/b.git/': SSL certificate problem: self-signed certificate", "TLS certificate"},
	} {
		err := transportError(&Error{Command: "push", Code: 128, stderr: tc.stderr}, nil)
		var te *TransportError
		if !errors.As(err, &te) || !strings.Contains(te.Cause, tc.cause) {
			t.Fatalf("%q: %v", tc.stderr, err)
		}
		if message := te.Error(); strings.Contains(message, "cd+ef") || strings.Contains(message, "pw/cd") || strings.Contains(message, "ab/cd") {
			t.Fatalf("password printed: %s", message)
		}
	}
}
