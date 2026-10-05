package overlay

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gitlocal/internal/storage"
)

const attrBegin = "# >>> git-local managed overlays >>>\n"
const attrEnd = "# <<< git-local managed overlays <<<\n"

func attrPattern(path string) string { return gitQuote(attrLiteral(path)) }

// legacyAttrPattern is the Go quoting of earlier releases. Git can't unquote
// its \u and \x escapes, but a block written with it is still recognized so
// the next add or remove can rewrite it.
func legacyAttrPattern(path string) string { return strconv.Quote(attrLiteral(path)) }

func attrLiteral(path string) string {
	var b strings.Builder
	b.WriteByte('/')
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '\\', '*', '?', '[', ']', '#', '!':
			b.WriteByte('\\')
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

// gitQuote produces the C-style quoting that Git's attribute parser accepts:
// \" and \\ escapes, octal escapes for control bytes and DEL, and every other
// byte, including UTF-8, unchanged.
func gitQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func attributeBlock(paths []string) ([]byte, error) {
	return renderBlock(paths, attrPattern)
}

func renderBlock(paths []string, pattern func(string) string) ([]byte, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	unique := map[string]string{}
	for _, p := range paths {
		if err := storage.ValidateSelectionPath(p); err != nil {
			return nil, err
		}
		key := storage.AliasKey(p)
		if old, ok := unique[key]; ok && old != p {
			return nil, errors.New("ambiguous overlay path aliases")
		}
		unique[key] = p
	}
	ordered := make([]string, 0, len(unique))
	for _, p := range unique {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	var b strings.Builder
	b.WriteString(attrBegin)
	for _, p := range ordered {
		b.WriteString(pattern(p))
		b.WriteString(" filter=gitlocal-overlay\n")
	}
	b.WriteString(attrEnd)
	return []byte(b.String()), nil
}

// AttributeUserBytes verifies the managed block against recorded paths and
// returns exactly the user's original attributes bytes.
func AttributeUserBytes(data []byte, oldPaths []string) ([]byte, error) {
	want, err := attributeBlock(oldPaths)
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		if bytes.Contains(data, []byte(attrBegin)) || bytes.Contains(data, []byte(attrEnd)) {
			return nil, errors.New("unowned managed overlay attributes block")
		}
		return bytes.Clone(data), nil
	}
	if !bytes.HasPrefix(data, want) {
		legacy, err := renderBlock(oldPaths, legacyAttrPattern)
		if err != nil || !bytes.HasPrefix(data, legacy) {
			return nil, errors.New("managed overlay attributes block was changed")
		}
		want = legacy
	}
	return bytes.Clone(data[len(want):]), nil
}

func RenderAttributes(user []byte, paths []string) ([]byte, error) {
	block, err := attributeBlock(paths)
	if err != nil {
		return nil, err
	}
	return append(block, user...), nil
}
