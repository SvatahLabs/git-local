package overlay

import (
	"errors"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const filterConfigName = "overlay-filter.gitconfig"

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func filterCommandForOS(executable, goos string) string {
	if goos == "windows" {
		// Git for Windows runs filter commands through a POSIX shell. Forward
		// slashes preserve the drive path and avoid shell backslash escaping.
		executable = strings.ReplaceAll(executable, "\\", "/")
	}
	return shellQuote(executable) + " overlay filter-process"
}

// FilterConfig is an extension-owned include file. Git's local config needs
// only one include.path entry, which Git itself can add/remove with its lock.
func FilterConfig(executable string) ([]byte, error) {
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\r\n") {
		return nil, errors.New("invalid overlay filter executable")
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(executable), ".exe") {
		return nil, errors.New("Windows overlay filter executable must end in .exe")
	}
	command := filterCommandForOS(executable, runtime.GOOS)
	return []byte("[filter \"gitlocal-overlay\"]\n\tprocess = " + strconv.Quote(command) + "\n\trequired = true\n"), nil
}
