package exclude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patterns, err := Compile(tc.states)
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
	names := []string{"a*?[]", ":(glob)*", "[!]", "-", "!", "#", "é", "è", "汉字", "a b", "a\tb", "x]y", "x[y", "x^y", "x-y"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			s := state([]selection.Rule{{Path: "notes", Kind: "directory"}}, "notes/"+name)
			body, err := Compile([]selection.State{s})
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
		if _, err := Compile(states); err == nil {
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
	if !Outdated(legacy, patterns) || Outdated(Render(user, patterns), patterns) || Outdated(user, nil) {
		t.Fatal("Outdated misreports the managed block")
	}
	if !Rendered(legacy, user, patterns) || !Rendered(Render(user, patterns), user, patterns) || Rendered(user, user, patterns) {
		t.Fatal("Rendered misreports the managed block")
	}
}
