package storage

import "testing"

// TestPortableAliasCollapsesSeparatorsAndStreams covers spellings Windows
// resolves to the same file: repeated separators and NTFS stream suffixes.
func TestPortableAliasCollapsesSeparatorsAndStreams(t *testing.T) {
	for path, want := range map[string]string{
		`notes\a.txt`:                    "notes/a.txt",
		`notes\\a.txt`:                   "notes/a.txt",
		`notes\/a.txt`:                   "notes/a.txt",
		"notes/a.txt::$DATA":             "notes/a.txt",
		"notes/a.txt:s:$DATA":            "notes/a.txt",
		"notes/a.txt:stream":             "notes/a.txt",
		"notes::$INDEX_ALLOCATION/a.txt": "notes/a.txt",
		"notes./a.txt. ":                 "notes/a.txt",
		"notes/a.txt":                    "notes/a.txt",
	} {
		if got := PortableAlias(path); got != want {
			t.Errorf("PortableAlias(%q) = %q, want %q", path, got, want)
		}
	}
}
