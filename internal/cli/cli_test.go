package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestCommandContract(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{{nil, 0}, {[]string{"--help"}, 0}, {[]string{"help"}, 0}, {[]string{"track"}, 2}, {[]string{"track", "--from", "", "path"}, 2}, {[]string{"track", "--from", "HEAD", "path"}, 2}, {[]string{"track", "--workspace", "bad", "path"}, 2}, {[]string{"workspace", "retire", "bad"}, 2}, {[]string{"workspace", "list", "extra"}, 2}, {[]string{"workspace", "prune", "extra"}, 2}, {[]string{"hook", "install", "extra"}, 2}, {[]string{"auto", "watch", "--interval", "0s"}, 2}, {[]string{"auto", "watch", "extra"}, 2}, {[]string{"status", "--porcelain"}, 2}, {[]string{"status", "-z"}, 2}, {[]string{"status", "path"}, 2}, {[]string{"diff", "path"}, 2}, {[]string{"log", "-n", "0"}, 2}, {[]string{"log", "-n", "1001"}, 2}, {[]string{"log", "--workspace", "x"}, 2}, {[]string{"commit"}, 2}, {[]string{"commit", "-m", ""}, 2}, {[]string{"commit", "-m", "message", "path"}, 2}, {[]string{"untrack"}, 2}, {[]string{"recover"}, 2}, {[]string{"recover", "bad", "--resume"}, 2}, {[]string{"recover", "--list", "--resume"}, 2}, {[]string{"unknown"}, 2}, {[]string{"--bogus"}, 2}, {[]string{"init", "unexpected"}, 2}, {[]string{"init", "--from", "bad"}, 2}, {[]string{"init", "--paths", "notes/a"}, 2}} {
		t.Run(stringArgs(tc.args), func(t *testing.T) {
			var out, err bytes.Buffer
			if code := Execute(context.Background(), tc.args, &out, &err); code != tc.code {
				t.Fatalf("got %d want %d: %s", code, tc.code, err.String())
			}
		})
	}
}
func stringArgs(a []string) string {
	if len(a) == 0 {
		return "empty"
	}
	return a[0]
}
func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var b bytes.Buffer
	if code := Execute(ctx, []string{"init"}, &b, &b); code != 1 {
		t.Fatal(code)
	}
}

func TestOverlayAddRequiresLiteralPathSeparator(t *testing.T) {
	for _, args := range [][]string{{"overlay", "add"}, {"overlay", "add", "name", "file"}, {"overlay", "add", "name", "--"}, {"overlay", "add", "name", "--", "file", "extra"}} {
		var out, diagnostic bytes.Buffer
		if code := Execute(context.Background(), args, &out, &diagnostic); code != 2 {
			t.Fatalf("%v: exit %d %s", args, code, diagnostic.String())
		}
	}
}

func TestRestoreUsage(t *testing.T) {
	for _, args := range [][]string{
		{"restore"},
		{"restore", "--dry-run", "notes"},
		{"restore", "--dry-run", "--source", "latest"},
		{"restore", "--dry-run", "--source", "HEAD", "notes"},
		{"restore", "--dry-run", "--source", "latest", "--workspace", "bad", "notes"},
	} {
		var out, err bytes.Buffer
		if code := Execute(context.Background(), args, &out, &err); code != 2 {
			t.Fatalf("%v: %d %s", args, code, err.String())
		}
	}
}

func TestDoctorAndCompletionContract(t *testing.T) {
	for _, args := range [][]string{{"doctor", "extra"}, {"doctor", "--bogus"}} {
		var out, err bytes.Buffer
		if code := Execute(context.Background(), args, &out, &err); code != 2 {
			t.Fatalf("%v: %d %s", args, code, err.String())
		}
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var out, err bytes.Buffer
		if code := Execute(context.Background(), []string{"completion", shell}, &out, &err); code != 0 || out.Len() == 0 {
			t.Fatalf("completion %s: %d %s", shell, code, err.String())
		}
	}
	var out, err bytes.Buffer
	if code := Execute(context.Background(), []string{"doctor", "--help"}, &out, &err); code != 0 || !bytes.Contains(out.Bytes(), []byte("--json")) {
		t.Fatal(code, out.String(), err.String())
	}
}

func TestVersionFlag(t *testing.T) {
	var out, diagnostic bytes.Buffer
	if code := Execute(context.Background(), []string{"--version"}, &out, &diagnostic); code != 0 || !bytes.Contains(out.Bytes(), []byte(Version)) {
		t.Fatal(code, out.String(), diagnostic.String())
	}
}

// GL-060: --version names the source commit a release build embeds.
func TestVersionFlagShowsCommit(t *testing.T) {
	savedVersion, savedCommit := Version, Commit
	defer func() { Version, Commit = savedVersion, savedCommit }()
	Version, Commit = "1.2.3", "0123456789abcdef0123456789abcdef01234567"
	var out, diagnostic bytes.Buffer
	if code := Execute(context.Background(), []string{"--version"}, &out, &diagnostic); code != 0 || out.String() != "git-local version 1.2.3 (commit 0123456789abcdef0123456789abcdef01234567)\n" {
		t.Fatalf("--version: exit %d %q %q", code, out.String(), diagnostic.String())
	}
}

func TestJSONEnvelopeForHelpVersionCompletionAndErrors(t *testing.T) {
	for _, args := range [][]string{{"--json", "--help"}, {"--json", "--version"}, {"completion", "bash", "--json"}, {"unknown", "--json"}, {"track", "--json"}, {"--json=1", "track"}} {
		var out, diagnostic bytes.Buffer
		code := Execute(context.Background(), args, &out, &diagnostic)
		if diagnostic.Len() != 0 {
			t.Fatalf("%v emitted non-JSON diagnostics: %s", args, diagnostic.String())
		}
		var envelope struct {
			SchemaVersion int             `json:"schemaVersion"`
			OK            bool            `json:"ok"`
			Data          json.RawMessage `json:"data"`
			Error         *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.SchemaVersion != 1 {
			t.Fatalf("%v invalid JSON envelope: %v %s", args, err, out.String())
		}
		if code == 0 && (!envelope.OK || len(envelope.Data) == 0) || code != 0 && (envelope.OK || envelope.Error == nil) {
			t.Fatalf("%v mismatched JSON status: exit %d %s", args, code, out.String())
		}
	}
}
