package gitexec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// TransportError is a failed backup push or fetch whose cause Git named.
// Error never holds Git's messages, which can name a URL with credentials;
// a TransportError adds the cause class and the line of Git's output that
// names it, with credentials in URLs redacted.
type TransportError struct {
	Err    *Error
	Cause  string
	Detail string
	Advice string
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("git %s failed (exit %d): %s (Git: %s); %s", e.Err.Command, e.Err.Code, e.Cause, e.Detail, e.Advice)
}
func (e *TransportError) Unwrap() error { return e.Err }

// transportClasses are checked in order; each matches lowercased lines of
// Git's output. A certificate failure also mentions the connection, so it
// comes first. A dead proxy shows only as a failed connection (to the
// proxy's address), so proxy and connection failures are one class.
var transportClasses = []struct {
	cause, advice string
	patterns      []string
}{
	{"the remote's TLS certificate isn't trusted, or the TLS connection failed", "if a private certificate authority signed it, set http.sslCAInfo (or http.<url>.sslCAInfo) to that authority's certificate in your global Git config",
		[]string{"ssl certificate problem", "certificate verify failed", "server certificate verification failed", "unable to get local issuer certificate", "self-signed certificate", "self signed certificate", "certificate has expired", "ssl_connect", "ssl connect error", "schannel", "gnutls", "tls handshake", "ssl: "}},
	{"authentication failed", "configure a credential helper (credential.helper or credential.<url>.helper in your global Git config) for an HTTPS remote, or an SSH key and known_hosts entry for an SSH remote",
		[]string{"authentication failed", "could not read username", "could not read password", "terminal prompts disabled", "permission denied (publickey", "host key verification failed", "invalid username or password", "returned error: 401"}},
	{"the remote or its proxy couldn't be reached", "check the URL, your network, and http.proxy (or http.<url>.proxy) in your global Git config",
		[]string{"proxy", "could not resolve host", "failed to connect", "couldn't connect", "connection refused", "timed out", "network is unreachable", "no route to host", "connection reset", "ssh: connect to host", "empty reply from server", "recv failure", "connection closed"}},
	{"the remote refused the request", "check that the remote repository exists, that you may write to it, and that it accepts refs/gitlocal/backups/ refs",
		[]string{"[remote rejected]", "[rejected]", "hook declined", "returned error: 403", "returned error: 404", "permission denied", "access denied", "not found", "does not appear to be a git repository", "couldn't find remote ref", "denied"}},
}

// credentials matches the user information of a URL.
var credentials = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@'"]+@`)

// redact replaces the user information of every URL in line and drops
// control characters.
func redact(line string) string {
	line = credentials.ReplaceAllString(line, "${1}***@")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, line)
}

// transportError names the cause of a failed backup transport from Git's
// stderr and output (push --porcelain reports a rejection on stdout). An
// unknown failure keeps the plain Error.
func transportError(err error, out []byte) error {
	var ge *Error
	if !errors.As(err, &ge) {
		return err
	}
	lines := strings.Split(ge.stderr+"\n"+string(out), "\n")
	for _, class := range transportClasses {
		for _, line := range lines {
			lower := strings.ToLower(line)
			for _, pattern := range class.patterns {
				if strings.Contains(lower, pattern) {
					detail := strings.TrimSpace(redact(line))
					if len(detail) > 300 {
						detail = strings.ToValidUTF8(detail[:300], "") + "…"
					}
					return &TransportError{Err: ge, Cause: class.cause, Detail: detail, Advice: class.advice}
				}
			}
		}
	}
	return err
}
