package backup

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// RemoteHost accepts only SSH and HTTPS Git transports. The caller must pass
// Git-expanded remote URLs (including insteadOf and pushInsteadOf rewrites).
func RemoteHost(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return "", errors.New("invalid backup remote URL")
	}
	host := ""
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "ssh" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" || u.Fragment != "" {
			return "", errors.New("backup remote must use SSH or HTTPS")
		}
		host = u.Hostname()
		if u.Path == "" || u.Path == "/" {
			return "", errors.New("backup remote repository path is missing")
		}
	} else {
		// Git's scp-like form is [user@]host:path. Require a remote path and
		// reject ambiguous local paths, URL schemes, and drive-letter paths.
		prefix, path, ok := strings.Cut(raw, ":")
		if !ok || path == "" || strings.HasPrefix(path, ":") || strings.Contains(prefix, "/") || strings.HasPrefix(prefix, ".") || strings.Contains(prefix, "\\") {
			return "", errors.New("backup remote must use SSH or HTTPS")
		}
		if at := strings.LastIndex(prefix, "@"); at >= 0 {
			if at == 0 || at == len(prefix)-1 {
				return "", errors.New("invalid SSH backup remote host")
			}
			prefix = prefix[at+1:]
		}
		if len(prefix) == 1 || strings.ContainsAny(prefix, "[]:@ ") {
			return "", errors.New("invalid SSH backup remote host")
		}
		host = prefix
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || strings.ContainsAny(host, " /\\@") || strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", errors.New("invalid backup remote host")
	}
	if host == "localhost" {
		return "", errors.New("backup remote must not be on this host")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return "", errors.New("backup remote must not be a loopback or unspecified address")
	}
	return host, nil
}

// ValidateDestination refuses origin and any destination on an origin host.
// URLs must be the effective fetch/push URLs returned by Git, not raw config.
func ValidateDestination(name string, destinationURLs, originURLs []string) error {
	if name == "" || name == "origin" || strings.ContainsAny(name, "\x00\r\n") {
		return errors.New("choose a named backup remote other than origin")
	}
	if len(destinationURLs) == 0 {
		return errors.New("backup remote has no destination URL")
	}
	originHosts := map[string]bool{}
	for _, raw := range originURLs {
		host, err := RemoteHost(raw)
		if err != nil {
			return fmt.Errorf("cannot verify origin host: %w", err)
		}
		originHosts[host] = true
	}
	for _, raw := range destinationURLs {
		host, err := RemoteHost(raw)
		if err != nil {
			return err
		}
		if originHosts[host] {
			return errors.New("backup remote shares a host with origin")
		}
	}
	return nil
}
