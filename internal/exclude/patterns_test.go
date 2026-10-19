package exclude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/testutil"
)

func state(include []selection.Rule, excluded ...string) selection.State {
	return selection.State{SchemaVersion: 2, Include: include, Exclude: excluded}
}
func TestEffectiveUnionAgainstGit(t *testing.T) {
	r := testutil.New(t)
	g, err := gitexec.New(r.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	cases := []struct {
		name   string
		states []selection.State
	}{
		{"directory", []selection.State{state([]selection.Rule{{Path: "notes", Kind: "directory"}})}},
		{"exception", []selection.State{state([]selection.Rule{{Path: "notes", Kind: "directory"}}, "notes/private")}},
		{"corridor", []selection.State{state([]selection.Rule{{Path: "notes", Kind: "directory"}}, "notes/private/deep/secret")}},
		{"union", []selection.State{state([]selection.Rule{{Path: "notes", Kind: "directory"}}, "notes/private"), state([]selection.Rule{{Path: "notes/private", Kind: "directory"}}, "notes/private/skip")}},
		{"filled-hole", []selection.State{state([]selection.Rule{{Path: "notes", Kind: "directory"}}, "notes/private"), state([]selection.Rule{{Path: "notes/private", Kind: "directory"}})}},
		{"literal", []selection.State{state([]selection.Rule{{Path: "notes", Kind: "directory"}}, "notes/space name", "notes/café", "notes/-dash", "notes/[x]", "notes/!bang", "notes/#hash")}},
	}
	paths := []string{"outside", "notes/private", "notes/private/secret", "notes/private/deep/secret", "notes/private/deep/secret/child", "notes/private/deep/other", "notes/private/skip", "notes/private/skip/child", "notes/private/skip-more", "notes/private-more", "notes/private.txt", "notes/p", "notes/pr", "notes/other", "notes/space name", "notes/space name/child", "notes/space name-more", "notes/café", "notes/café/child", "notes/cafè", "notes/caféteria", "notes/-dash", "notes/-dash-long", "notes/[x]", "notes/[y]", "notes/!bang", "notes/!bang-more", "notes/#hash", "notes/with\ttab"}
	// Build directories before checking descendants, so Git uses the real topology.
	for _, p := range paths {
		if strings.Contains(p, "/") {
			os.MkdirAll(filepath.Dir(filepath.Join(r.Root, p)), 0700)
		}
	}
	os.MkdirAll(filepath.Join(r.Root, ".git/info"), 0700)
	ignoreCase, err := g.IgnoreCase(context.Background(), r.Root, filepath.Join(r.Root, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patterns, err := Compile(tc.states, ignoreCase)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range bytes.Split(patterns, []byte{'\n'}) {
				if len(line) > 0 && line[0] == '!' {
					t.Fatal("generated negation")
				}
			}
			if err = os.WriteFile(filepath.Join(r.Root, ".git/info/exclude"), patterns, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := g.CheckIgnore(context.Background(), r.Root, filepath.Join(r.Root, ".git"), paths)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range paths {
				want := false
				for _, s := range tc.states {
					if s.Selected(p) {
						want = true
					}
				}
				// A partially selected directory is deliberately a traversable corridor,
				// not itself an ignored file. Only compare file paths or wholly covered nodes.
				fi, e := os.Stat(filepath.Join(r.Root, p))
				if e == nil && fi.IsDir() {
					continue
				}
				if got[p] != want {
					t.Errorf("%q ignored=%v want=%v\n%s", p, got[p], want, patterns)
				}
			}
		})
	}
}
func TestLiteralPatternBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("names reserved on Windows")
	}
	r := testutil.New(t)
	g, err := gitexec.New(r.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	os.MkdirAll(filepath.Join(r.Root, ".git/info"), 0700)
	ignoreCase, err := g.IgnoreCase(context.Background(), r.Root, filepath.Join(r.Root, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"a*?[]", ":(glob)*", "[!]", "-", "!", "#", "é", "è", "汉字", "a b", "a\tb", "x]y", "x[y", "x^y", "x-y"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			s := state([]selection.Rule{{Path: "notes", Kind: "directory"}}, "notes/"+name)
			body, err := Compile([]selection.State{s}, ignoreCase)
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(r.Root, ".git/info/exclude"), body, 0600)
			paths := []string{"notes/" + name, "notes/" + name + "/child", "notes/" + name + "more", "notes/other", "notes/汉语", "notes/café"}
			for i := 1; i < len(name); i++ {
				paths = append(paths, "notes/"+name[:i])
			}
			got, err := g.CheckIgnore(context.Background(), r.Root, filepath.Join(r.Root, ".git"), paths)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range paths {
				if got[p] != s.Selected(p) {
					t.Errorf("%q: got %v want %v\n%s", p, got[p], s.Selected(p), body)
				}
			}
		})
	}
}
func TestBlockPreservesUserBytes(t *testing.T) {
	user := []byte("# user\r\n*.tmp\nno-final-newline")
	patterns := []byte("/notes\n")
	rendered := Render(user, patterns)
	got, err := UserBytes(rendered, patterns)
	if err != nil || !bytes.Equal(got, user) {
		t.Fatal("user content changed", err)
	}
	for _, bad := range [][]byte{user, append(rendered, []byte(Begin)...), []byte(Begin + "/edited\n" + End), []byte(End), []byte("prefix" + Begin + "/notes\n" + End)} {
		if _, err := UserBytes(bad, patterns); err == nil {
			t.Fatal("accepted malformed/edited managed block")
		}
	}
	if got, err = UserBytes(user, nil); err != nil || !bytes.Equal(got, user) {
		t.Fatal(err)
	}
}
func TestIncompatibleWorkspaceRulesFail(t *testing.T) {
	cases := [][]selection.State{
		{state([]selection.Rule{{Path: "Notes", Kind: "directory"}}), state([]selection.Rule{{Path: "notes", Kind: "directory"}})},
		{state([]selection.Rule{{Path: "notes", Kind: "file"}}), state([]selection.Rule{{Path: "notes/child", Kind: "file"}})},
	}
	for _, states := range cases {
		if _, err := Compile(states, false); err == nil {
			t.Fatal("accepted ambiguous union")
		}
	}
}

func TestManagedBlockIgnoresStagesAndAcceptsLegacyBlock(t *testing.T) {
	patterns := []byte("/notes/secret\n")
	block := Block(patterns)
	if !bytes.Contains(block, []byte("\n"+StagePattern+"\n")) {
		t.Fatalf("managed block lacks the stage rule: %q", block)
	}
	user := []byte("*.log\n")
	legacy := append(append(append([]byte(Begin), patterns...), End...), user...)
	if got, err := UserBytes(legacy, patterns); err != nil || !bytes.Equal(got, user) {
		t.Fatalf("legacy block rejected: %v %q", err, got)
	}
	if !Outdated(legacy, patterns) || Outdated(Render(user, patterns), patterns) || !Outdated(user, nil) || Outdated(Render(user, nil), nil) {
		t.Fatal("Outdated misreports the managed block")
	}
	if !Rendered(legacy, user, patterns) || !Rendered(Render(user, patterns), user, patterns) || Rendered(user, user, patterns) {
		t.Fatal("Rendered misreports the managed block")
	}
}

// GL-130: Kept leaves the block's patterns, with StagePattern, as ordinary
// lines between KeptHeader and KeptEnd where the block was, adds them only
// once, and leaves nothing for a legacy block with no patterns.
func TestKeptLinesReplaceTheBlock(t *testing.T) {
	patterns := []byte("/private/\n/.env.local\n")
	kept := KeptHeader + StagePattern + "\n" + string(patterns) + KeptEnd
	if got := string(Kept(Render([]byte("/mine/\n!/private/x\n"), patterns))); got != kept+"/mine/\n!/private/x\n" {
		t.Fatalf("kept lines: %q", got)
	}
	if got := string(Kept(Render([]byte(kept+"/mine/\n"), patterns))); got != kept+"/mine/\n" {
		t.Fatalf("kept lines added twice: %q", got)
	}
	if got := string(Kept(append(legacyBlock(patterns), "/mine/\n"...))); got != KeptHeader+string(patterns)+KeptEnd+"/mine/\n" {
		t.Fatalf("kept lines of a legacy block: %q", got)
	}
	if got := string(Kept(Render([]byte("/mine/\n"), nil))); got != KeptHeader+StagePattern+"\n"+KeptEnd+"/mine/\n" {
		t.Fatalf("kept lines of an empty block: %q", got)
	}
}

// GL-130 review: WithoutKept removes exactly the sections Kept wrote, every
// one of them, and nothing a user wrote: a header without its end line, or
// with a comment or blank line before it, isn't a section.
func TestWithoutKeptRemovesExactSections(t *testing.T) {
	kept := string(Kept(Render([]byte("/mine/\n"), []byte("/private/\n"))))
	other := KeptHeader + "/a\n/b\n" + KeptEnd
	data := "/first\n" + other + kept + "/last\n"
	if got, removed := WithoutKept([]byte(data)); !removed || string(got) != "/first\n/mine/\n/last\n" {
		t.Fatalf("WithoutKept(%q) = %q, %v", data, got, removed)
	}
	for _, data := range []string{
		"/mine/\n",
		KeptHeader + "/a\n",
		KeptHeader + "/a\n# mine\n" + KeptEnd,
		KeptHeader + "/a\n\n" + KeptEnd,
		KeptHeader + "/a\n" + Begin + KeptEnd,
		"x" + KeptHeader + "/a\n" + KeptEnd,
		KeptHeader + "/a\nx" + KeptEnd,
	} {
		if got, removed := WithoutKept([]byte(data)); removed || string(got) != data {
			t.Fatalf("WithoutKept(%q) = %q, %v", data, got, removed)
		}
	}
	if got, removed := WithoutKept([]byte(KeptHeader + "/a\n" + KeptHeader + "/b\n" + KeptEnd)); !removed || string(got) != KeptHeader+"/a\n" {
		t.Fatalf("a header without its end line: %q, %v", got, removed)
	}
}

// GL-130 review: Render drops from kept sections the lines its block holds,
// and a section left empty, and keeps every other kept line, so a path never
// selected again stays ignored.
func TestRenderTakesBackCoveredKeptLines(t *testing.T) {
	section := KeptHeader + StagePattern + "\n/.env\n/private/\n" + KeptEnd
	user := "/first\n" + section + "/mine/\n"
	if got := string(Render([]byte(user), nil)); got != string(Block(nil))+"/first\n"+KeptHeader+"/.env\n/private/\n"+KeptEnd+"/mine/\n" {
		t.Fatalf("an empty block: %q", got)
	}
	if got := string(Render([]byte(user), []byte("/private/\n"))); got != string(Block([]byte("/private/\n")))+"/first\n"+KeptHeader+"/.env\n"+KeptEnd+"/mine/\n" {
		t.Fatalf("a block holding one kept line: %q", got)
	}
	if got := string(Render([]byte(user), []byte("/.env\n/private/\n"))); got != string(Block([]byte("/.env\n/private/\n")))+"/first\n/mine/\n" {
		t.Fatalf("a block holding every kept line: %q", got)
	}
	if _, removed := Covered([]byte(user), Block(nil)); len(removed) != 0 {
		t.Fatalf("Covered reports the stage pattern: %q", removed)
	}
	if _, removed := Covered([]byte(user), Block([]byte("/.env\n"))); len(removed) != 1 || removed[0] != "/.env" {
		t.Fatalf("Covered reports %q", removed)
	}
	if got := string(Render([]byte("/.env\n"), []byte("/.env\n"))); got != string(Block([]byte("/.env\n")))+"/.env\n" {
		t.Fatalf("Render dropped a user line: %q", got)
	}
}
