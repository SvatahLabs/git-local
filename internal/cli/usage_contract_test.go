package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// usageInvocations holds, for every runnable command, arguments that the
// command must reject as invalid usage before doing any work.
var usageInvocations = map[string][][]string{
	"git-local":                        {{"instal"}},
	"git-local auto event":             {{"post-commit", "extra"}},
	"git-local auto install":           {{"extra"}},
	"git-local auto status":            {{"extra"}},
	"git-local auto uninstall":         {{"extra"}},
	"git-local auto watch":             {{"extra"}},
	"git-local backup keygen":          {{"/tmp/key", "extra"}},
	"git-local backup push":            {{"backup", "extra", "--key-file", "/tmp/key"}},
	"git-local backup recover":         {{"extra"}},
	"git-local backup restore":         {{"backup", "refs/x", "extra", "--key-file", "/tmp/key"}},
	"git-local commit":                 {{"-m", "message", "extra"}},
	"git-local completion bash":        {{"extra"}},
	"git-local completion fish":        {{"extra"}},
	"git-local completion powershell":  {{"extra"}},
	"git-local completion zsh":         {{"extra"}},
	"git-local diff":                   {{"latest", "latest", "extra"}},
	"git-local doctor":                 {{"extra"}},
	"git-local encrypt enable":         {{"notes"}},
	"git-local encrypt keygen":         {{"/tmp/key", "extra"}},
	"git-local encrypt migrate":        {{"notes"}},
	"git-local encrypt rebind-key":     {{"/tmp/key", "extra"}},
	"git-local encrypt recover":        {{"bad", "--resume"}, {"00000000-0000-4000-8000-000000000000", "extra", "--resume"}},
	"git-local grep":                   {{"needle", "extra"}},
	"git-local guard push":             {{"origin", "extra"}},
	"git-local guard staged":           {{"extra"}},
	"git-local hook install":           {{"extra"}},
	"git-local hook status":            {{"extra"}},
	"git-local hook uninstall":         {{"extra"}},
	"git-local init":                   {{"extra"}},
	"git-local log":                    {{"extra"}},
	"git-local overlay add":            {{"name", "--", "file", "extra"}},
	"git-local overlay filter-process": {{"extra"}},
	"git-local overlay remove":         {{"name", "extra"}, {"name", "--workspace", "bad"}},
	"git-local overlay show":           {{"name", "extra"}},
	"git-local overlay status":         {{"extra"}},
	"git-local recover":                {{"bad", "--resume"}, {"--list", "extra"}},
	"git-local reset":                  {{"--hard", "HEAD", "extra"}},
	"git-local restore":                {{"--source", "latest", "--workspace", "bad", "--", "notes"}},
	"git-local show":                   {{"latest:notes", "extra"}},
	"git-local status":                 {{"extra"}},
	"git-local track":                  {{"--workspace", "bad", "--from", "latest", "notes"}},
	"git-local untrack":                {{}},
	"git-local workspace list":         {{"extra"}},
	"git-local workspace prune":        {{"extra"}},
	"git-local workspace remove":       {{"one", "two"}},
	"git-local workspace retire":       {{"bad"}},
}

// TestEveryCommandReportsInvalidUsageAsUsage walks the whole command tree:
// a mistyped subcommand, extra arguments and a malformed UUID must exit 2 and
// report error.code "usage" in JSON, without running the command.
func TestEveryCommandReportsInvalidUsageAsUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Run outside any repository, so a command that wrongly accepts its
	// arguments fails harmlessly instead of acting on this checkout.
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)

	var cases [][]string
	seen := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := c.CommandPath()
		words := strings.Fields(path)[1:]
		switch {
		case c.Name() == "help":
		case c.HasSubCommands() && c.HasParent():
			cases = append(cases, append(append([]string{}, words...), "instal"))
		default:
			invocations, ok := usageInvocations[path]
			if !ok {
				t.Errorf("%s has no invalid-usage case", path)
			}
			seen[path] = true
			for _, extra := range invocations {
				cases = append(cases, append(append([]string{}, words...), extra...))
			}
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	root := New(new(bytes.Buffer), new(bytes.Buffer))
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	walk(root)
	for path := range usageInvocations {
		if !seen[path] {
			t.Errorf("invalid-usage case for unknown command %s", path)
		}
	}
	for _, args := range cases {
		var out, diagnostic bytes.Buffer
		if code := Execute(context.Background(), args, &out, &diagnostic); code != 2 {
			t.Errorf("%q: exit %d, want 2: %s%s", args, code, out.String(), diagnostic.String())
			continue
		}
		if !strings.Contains(diagnostic.String(), "git local:") {
			t.Errorf("%q: no usage diagnostic: %q", args, diagnostic.String())
		}
		out.Reset()
		diagnostic.Reset()
		code := Execute(context.Background(), append([]string{"--json"}, args...), &out, &diagnostic)
		var envelope struct {
			OK    bool `json:"ok"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || code != 2 || envelope.OK || envelope.Error == nil || envelope.Error.Code != "usage" {
			t.Errorf("%q --json: exit %d, envelope %s (%v)", args, code, out.String(), err)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a rejected command touched the working directory: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatal("a rejected command created a repository")
	}
}

// TestMistypedSubcommandSuggestsTheCommand checks the human diagnostic.
func TestMistypedSubcommandSuggestsTheCommand(t *testing.T) {
	var out, diagnostic bytes.Buffer
	if code := Execute(context.Background(), []string{"hook", "instal"}, &out, &diagnostic); code != 2 {
		t.Fatalf("exit %d: %s", code, diagnostic.String())
	}
	if !strings.Contains(diagnostic.String(), `unknown command "instal" for "git local hook"`) || !strings.Contains(diagnostic.String(), "install") {
		t.Fatalf("diagnostic %q", diagnostic.String())
	}
	out.Reset()
	diagnostic.Reset()
	if code := Execute(context.Background(), []string{"hook"}, &out, &diagnostic); code != 0 || !strings.Contains(out.String(), "install") {
		t.Fatalf("group help: exit %d %q %q", code, out.String(), diagnostic.String())
	}
}
