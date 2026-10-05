package overlay

import (
	"bytes"
	"errors"
	"sort"
	"strconv"
	"strings"

	"gitlocal/internal/storage"
)

const attrBegin = "# >>> git-local managed overlays >>>\n"
const attrEnd = "# <<< git-local managed overlays <<<\n"

func attrPattern(path string) string {
	var b strings.Builder
	b.WriteByte('/')
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '\\', '*', '?', '[', ']', '#', '!':
			b.WriteByte('\\')
		}
		b.WriteByte(path[i])
	}
	return strconv.Quote(b.String())
}

func attributeBlock(paths []string) ([]byte, error) {
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
		b.WriteString(attrPattern(p))
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
		return nil, errors.New("managed overlay attributes block was changed")
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
