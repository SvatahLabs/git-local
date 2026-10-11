package backup

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// RemoteHost accepts only SSH and HTTPS Git transports. The caller must pass
// Git-expanded remote URLs (including insteadOf and pushInsteadOf rewrites).
func RemoteHost(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") || strings.HasPrefix(raw, "-") {
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
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " /\\@") || strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", errors.New("invalid backup remote host")
	}
	// One spelling per address, so the same-host rule can't be bypassed
	// with another IPv6 form, an IPv4-mapped address, or a form the OS
	// resolver reads as IPv4 (127.1, 0x7f.0.0.1, 2130706433).
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else if ip, ok := resolverIPv4(host); ok {
		host = ip.String()
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

// resolverIPv4 parses the IPv4 forms inet_aton accepts and resolvers use for
// host names: one to four dot-separated parts, each decimal, octal (leading
// 0) or hexadecimal (0x), the last part filling the remaining bytes.
func resolverIPv4(host string) (net.IP, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil, false
	}
	values := make([]uint64, len(parts))
	for i, part := range parts {
		base, digits := 10, part
		if len(part) > 2 && (part[:2] == "0x" || part[:2] == "0X") {
			base, digits = 16, part[2:]
		} else if len(part) > 1 && part[0] == '0' {
			base, digits = 8, part[1:]
		}
		if digits == "" || strings.ContainsAny(digits, "+-_") {
			return nil, false
		}
		v, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			return nil, false
		}
		values[i] = v
	}
	last := len(values) - 1
	var addr uint64
	for i, v := range values[:last] {
		if v > 255 {
			return nil, false
		}
		addr |= v << (24 - 8*i)
	}
	if values[last] >= 1<<(32-8*last) {
		return nil, false
	}
	addr |= values[last]
	return net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr)), true
}
