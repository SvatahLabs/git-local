package main

import (
	"os"
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

// GL-078: every relative link in committed Markdown resolves in a clone,
// including its #anchor, so no document points into local-only files.
func TestCommittedMarkdownLinksResolve(t *testing.T) {
	root := ".."
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(data), -1) {
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
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s links to missing %s", path, link)
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
			for _, heading := range headingLine.FindAllStringSubmatch(string(doc), -1) {
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
