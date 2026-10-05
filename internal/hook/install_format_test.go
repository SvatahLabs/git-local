package hook

import (
	"bytes"
	"testing"
)

func TestWindowsHookBlockUsesQuotedExeAndFileCheck(t *testing.T) {
	path := `C:\Program Files\Atul's GitLocal\git-local.exe`
	quoted := `'C:/Program Files/Atul'\''s GitLocal/git-local.exe'`
	for _, name := range []string{"pre-commit", "pre-push", "post-commit", "post-checkout", "pre-rebase", "post-rewrite"} {
		block := blockForOS(name, path, "windows")
		if !bytes.Contains(block, []byte("if [ ! -f "+quoted+" ]; then\n")) || !bytes.Contains(block, []byte(quoted+" ")) || bytes.Contains(block, []byte("[ ! -x ")) {
			t.Fatalf("%s does not use a Windows shell-safe executable path:\n%s", name, block)
		}
		posix := blockForOS(name, "/tmp/git-local", "linux")
		if !bytes.Contains(posix, []byte("if [ ! -x '/tmp/git-local' ]; then\n")) {
			t.Fatalf("%s changed the POSIX executable check:\n%s", name, posix)
		}
	}
}
