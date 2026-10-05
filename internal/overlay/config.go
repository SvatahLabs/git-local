package overlay

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gitlocal/internal/gitexec"
)

const filterConfigName = "overlay-filter.gitconfig"

// filterInclude is resolved by Git relative to the config file holding the
// include, so it keeps working when the checkout moves or is mounted elsewhere.
const filterInclude = "gitlocal/" + filterConfigName

// requiredKey is also set directly in the local config, so a missing or
// unreadable include fails closed instead of leaving an undefined driver.
const requiredKey = "filter.gitlocal-overlay.required"

// managedIncludes counts the relative include and returns absolute includes
// written by earlier releases, which stop resolving when the checkout moves.
func managedIncludes(includes []string) (int, []string) {
	count, legacy := 0, []string{}
	for _, item := range includes {
		if item == filterInclude {
			count++
		} else if filepath.IsAbs(item) && strings.HasSuffix(filepath.ToSlash(item), "/"+filterInclude) {
			legacy = append(legacy, item)
		}
	}
	return count, legacy
}

// localRequired reports whether the local config sets the direct required
// key, and refuses any value other than true.
func localRequired(ctx context.Context, r *gitexec.Runner, root string) (bool, error) {
	b, err := localConfig(ctx, r, root, "--bool", "--get-all", requiredKey)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	if string(b) != "true\n" {
		return true, errors.New("local Git config sets " + requiredKey + " to a value other than true")
	}
	return true, nil
}

// unsetLocal removes every local value of key; an absent key is not an error.
func unsetLocal(ctx context.Context, r *gitexec.Runner, root string, args ...string) error {
	_, err := localConfig(ctx, r, root, append([]string{"--unset-all"}, args...)...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 5 {
		return nil
	}
	return err
}

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
// only one relative include.path entry and the direct required key, which Git
// itself can add/remove with its lock.
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
