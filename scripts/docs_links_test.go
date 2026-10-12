package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// localOnly are directories that exist only in a working checkout (ignored by
// mainline Git), never in a clone.
var localOnly = map[string]bool{".git": true, ".claude": true, "bin": true, "dist": true, "project": true, "tracker": true}

var (
	markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	headingLine  = regexp.MustCompile(`(?m)^#{1,6} +(.+)$`)
)

// anchor is GitHub's heading slug: lower case, punctuation other than
// hyphens and underscores dropped, spaces turned into hyphens.
func anchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// withoutFences drops fenced code blocks, whose lines are neither links nor
// headings.
func withoutFences(text string) string {
	var b strings.Builder
	fenced := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if !fenced {
			b.WriteString(line)
		}
	}
	return b.String()
}

// committedFiles lists the files Git tracks under root, relative to it with
// forward slashes, or returns nil outside a Git checkout (an archive holds
// only committed files anyway).
func committedFiles(t *testing.T, root string) map[string]bool {
	t.Helper()
	top, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(root); err != nil || filepath.Clean(strings.TrimSpace(string(top))) != resolved {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		files[name] = true
		for dir, _, ok := cutLast(name); ok; dir, _, ok = cutLast(dir) {
			files[dir] = true
		}
	}
	return files
}

// cutLast splits a slash-separated path at its last slash.
func cutLast(name string) (string, string, bool) {
	i := strings.LastIndex(name, "/")
	if i < 0 {
		return "", name, false
	}
	return name[:i], name[i+1:], true
}

// GL-078: every relative link in committed Markdown resolves in a clone,
// including its #anchor, so no document points into local-only files.
func TestCommittedMarkdownLinksResolve(t *testing.T) {
	root := ".."
	committed := committedFiles(t, root)
	checked := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && localOnly[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		if rel, err := filepath.Rel(root, path); err != nil || committed != nil && !committed[filepath.ToSlash(rel)] {
			return err // Not committed: local-only.
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllStringSubmatch(withoutFences(string(data)), -1) {
			link := match[1]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			target, fragment, _ := strings.Cut(link, "#")
			resolved := path
			if target != "" {
				resolved = filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
				rel, err := filepath.Rel(root, resolved)
				if err != nil || strings.HasPrefix(rel, "..") || localOnly[strings.Split(filepath.ToSlash(rel), "/")[0]] {
					t.Errorf("%s links to %s, outside the committed files", path, link)
					continue
				}
				if _, err := os.Stat(resolved); err != nil || committed != nil && !committed[filepath.ToSlash(rel)] {
					t.Errorf("%s links to missing or uncommitted %s", path, link)
					continue
				}
			}
			checked++
			if fragment == "" || !strings.HasSuffix(resolved, ".md") {
				continue
			}
			doc, err := os.ReadFile(resolved)
			if err != nil {
				return err
			}
			found := false
			for _, heading := range headingLine.FindAllStringSubmatch(withoutFences(string(doc)), -1) {
				if anchor(heading[1]) == fragment {
					found = true
				}
			}
			if !found {
				t.Errorf("%s links to %s, which has no heading #%s", path, target, fragment)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no Markdown links found")
	}
}
