// Package exclude compiles the union of effective selections into positive-only
// Git ignore patterns. No generated rule can unignore a user's existing ignore.
package exclude

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
)

const Begin = "# >>> git-local managed exclusions >>>\n"
const End = "# <<< git-local managed exclusions <<<\n"
const MaxPatterns = 128 << 10

type node struct {
	path     string
	children map[string]*node
}

func (n *node) add(path string) {
	cur := n
	for _, part := range strings.Split(path, "/") {
		if cur.children == nil {
			cur.children = map[string]*node{}
		}
		child := cur.children[part]
		if child == nil {
			p := part
			if cur.path != "" {
				p = cur.path + "/" + part
			}
			child = &node{path: p}
			cur.children[part] = child
		}
		cur = child
	}
}
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '*', '?', '[', ']', ' ', '#', '!':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Compile uses a path trie to combine workspaces before generating rules.
// Literal exclusions create traversable corridors. At an included corridor,
// byte-prefix glob partitions match all sibling names except explicit branches.
// Git wildmatch operates on bytes, so even UTF-8 literal exceptions partition
// correctly without needing negations or enumerating future files.
func Compile(states []selection.State) ([]byte, error) {
	normalized := []selection.State{}
	root := &node{}
	aliases := map[string]string{}
	for _, s := range states {
		n, err := selection.Canonical(s)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, n)
		paths := append([]string{}, n.Exclude...)
		for _, r := range n.Include {
			paths = append(paths, r.Path)
		}
		for _, p := range paths {
			parts := strings.Split(p, "/")
			for i := range parts {
				prefix := strings.Join(parts[:i+1], "/")
				key := storage.AliasKey(prefix)
				if old, ok := aliases[key]; ok && old != prefix {
					return nil, fmt.Errorf("%w: incompatible workspace path aliases %q and %q", storage.ErrConflict, old, prefix)
				}
				aliases[key] = prefix
			}
			root.add(p)
		}
	}
	var output bytes.Buffer
	emit := func(p string) error {
		output.WriteString(p)
		output.WriteByte('\n')
		if output.Len() > MaxPatterns {
			return errors.New("managed ignore rules exceed the size limit")
		}
		return nil
	}
	var walk func(*node) error
	walk = func(n *node) error {
		directory, file := false, false
		for _, s := range normalized {
			if s.Excluded(n.path) {
				continue
			}
			for _, r := range s.Include {
				if r.Kind == "directory" && (r.Path == n.path || strings.HasPrefix(n.path, r.Path+"/")) {
					directory = true
				}
				if r.Kind == "file" && r.Path == n.path {
					file = true
				}
			}
		}
		if len(n.children) == 0 {
			if directory || file {
				return emit("/" + escape(n.path))
			}
			return nil
		}
		if file {
			return fmt.Errorf("%w: shared ignore rules cannot represent both file and directory selections at %q", storage.ErrConflict, n.path)
		}
		children := make([]string, 0, len(n.children))
		for name := range n.children {
			children = append(children, name)
		}
		sort.Strings(children)
		if directory {
			if err := siblings(children, func(pattern string) error { return emit("/" + escape(n.path) + "/" + pattern) }); err != nil {
				return err
			}
		}
		for _, name := range children {
			if err := walk(n.children[name]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type chars struct {
	terminal bool
	children map[byte]*chars
}

func siblings(exceptions []string, emit func(string) error) error {
	root := &chars{}
	for _, s := range exceptions {
		n := root
		for i := 0; i < len(s); i++ {
			if n.children == nil {
				n.children = map[byte]*chars{}
			}
			if n.children[s[i]] == nil {
				n.children[s[i]] = &chars{}
			}
			n = n.children[s[i]]
		}
		n.terminal = true
	}
	var walk func(*chars, string) error
	walk = func(n *chars, prefix string) error {
		if prefix != "" && !n.terminal {
			if err := emit(escape(prefix)); err != nil {
				return err
			}
		}
		if len(n.children) == 0 {
			if prefix == "" {
				return emit("*")
			}
			return emit(escape(prefix) + "?*")
		}
		keys := []int{}
		for c := range n.children {
			keys = append(keys, int(c))
		}
		sort.Ints(keys)
		var class strings.Builder
		class.WriteString("[!")
		for _, c := range keys {
			if c == ']' || c == '-' || c == '\\' || c == '^' {
				class.WriteByte('\\')
			}
			class.WriteByte(byte(c))
		}
		class.WriteString("]*")
		if err := emit(escape(prefix) + class.String()); err != nil {
			return err
		}
		for _, c := range keys {
			if err := walk(n.children[byte(c)], prefix+string([]byte{byte(c)})); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, "")
}
// StagePattern ignores restore and seed staging files at any depth. An
// interrupted operation keeps them beside a selected file, where the file's own
// rule doesn't match, and they can hold decrypted content.
const StagePattern = ".gitlocal-tmp-*"

func Block(patterns []byte) []byte {
	if len(patterns) == 0 {
		return nil
	}
	b := append([]byte(Begin), StagePattern+"\n"...)
	b = append(b, patterns...)
	return append(b, []byte(End)...)
}

// legacyBlock is the block written by releases without StagePattern. It stays
// accepted so existing repositories keep working; the next selection change
// rewrites it.
func legacyBlock(patterns []byte) []byte {
	if len(patterns) == 0 {
		return nil
	}
	b := append([]byte(Begin), patterns...)
	return append(b, []byte(End)...)
}

// Outdated reports a valid managed block that lacks StagePattern.
func Outdated(data, expected []byte) bool {
	if len(expected) == 0 {
		return false
	}
	_, err := UserBytes(data, expected)
	return err == nil && !bytes.Contains(data, Block(expected))
}

// Rendered reports whether data is user followed by the current or legacy
// managed block for patterns.
func Rendered(data, user, patterns []byte) bool {
	return bytes.Equal(data, Render(user, patterns)) || bytes.Equal(data, append(legacyBlock(patterns), user...))
}

// UserBytes verifies the exact managed block and returns unchanged bytes outside
// it. Missing/duplicate/edited markers fail closed. New blocks go before user
// content, so later user negations keep their normal precedence.
func UserBytes(data, expected []byte) ([]byte, error) {
	begin, end := []byte(strings.TrimSuffix(Begin, "\n")), []byte(strings.TrimSuffix(End, "\n"))
	a, b := bytes.Count(data, begin), bytes.Count(data, end)
	if a == 0 && b == 0 {
		if len(expected) != 0 {
			return nil, fmt.Errorf("%w: managed ignore block is missing", storage.ErrConflict)
		}
		return bytes.Clone(data), nil
	}
	if a != 1 || b != 1 {
		return nil, fmt.Errorf("%w: duplicate or incomplete managed ignore markers", storage.ErrConflict)
	}
	start := bytes.Index(data, []byte(Begin))
	stop := bytes.Index(data, []byte(End))
	if start < 0 || stop < start || start > 0 && data[start-1] != '\n' {
		return nil, fmt.Errorf("%w: malformed managed ignore block", storage.ErrConflict)
	}
	stop += len(End)
	if !bytes.Equal(data[start:stop], Block(expected)) && !bytes.Equal(data[start:stop], legacyBlock(expected)) {
		return nil, fmt.Errorf("%w: managed ignore block was edited", storage.ErrConflict)
	}
	result := append([]byte{}, data[:start]...)
	return append(result, data[stop:]...), nil
}
func Render(user, patterns []byte) []byte { b := Block(patterns); return append(b, user...) }
