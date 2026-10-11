package backup

import (
	"strings"
	"testing"
)

func TestRemoteHostParsesEffectiveGitURLs(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user@example.com:443/owner/repo.git": "example.com",
		"ssh://git@EXAMPLE.COM:22/owner/repo.git":     "example.com",
		"git@example.com:owner/repo.git":              "example.com",
		"example.com:owner/repo.git":                  "example.com",
		"ssh://git@[2001:db8::1]/owner/repo.git":      "2001:db8::1",
	} {
		got, err := RemoteHost(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "../backup.git", "/tmp/backup.git", "file:///tmp/backup.git", "http://example.com/repo", "https://localhost/repo", "ssh://127.0.0.1/repo", "ssh://[::1]/repo", "C:\\backup", "ext::helper", "https://example.com", "git@example.com:"} {
		if _, err := RemoteHost(raw); err == nil {
			t.Fatalf("unsafe backup remote %q accepted", raw)
		}
	}
}

func TestDestinationRejectsOriginAndSameHost(t *testing.T) {
	origin := []string{"git@github.example:project/source.git", "ssh://git@mirror.example/project/source.git"}
	for _, tc := range []struct {
		name string
		urls []string
	}{
		{"origin", []string{"ssh://git@backup.example/project/backup.git"}},
		{"backup", []string{"https://GITHUB.EXAMPLE/project/backup.git"}},
		{"backup", []string{"ssh://git@other.example/project/backup.git", "git@mirror.example:project/backup.git"}},
		{"backup", []string{"/tmp/local.git"}},
	} {
		if err := ValidateDestination(tc.name, tc.urls, origin); err == nil {
			t.Fatalf("unsafe destination %+v accepted", tc)
		}
	}
	if err := ValidateDestination("backup", []string{"ssh://git@backup.example/project/backup.git"}, origin); err != nil {
		t.Fatal("distinct backup host rejected", err)
	}
}

func TestRemoteHostCanonicalizesIPsAndRejectsOptionLikeHosts(t *testing.T) {
	for raw, want := range map[string]string{
		"ssh://git@[2001:db8:0::1]/owner/repo.git":        "2001:db8::1",
		"https://[2001:DB8:0000:0000:0000:0000:0000:1]/r": "2001:db8::1",
		"ssh://git@[::ffff:192.0.2.1]/owner/repo.git":     "192.0.2.1",
		"git@192.0.2.1:owner/repo.git":                    "192.0.2.1",
		"https://Backup.Example./owner/repo.git":          "backup.example",
	} {
		if got, err := RemoteHost(raw); err != nil || got != want {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	origin := []string{"ssh://git@[2001:db8::1]/project/source.git"}
	for _, backup := range []string{"ssh://git@[2001:db8:0::1]/project/backup.git", "https://[2001:0db8::0001]/project/backup.git"} {
		if err := ValidateDestination("backup", []string{backup}, origin); err == nil {
			t.Fatalf("backup %q on the origin host accepted", backup)
		}
	}
	for _, raw := range []string{"-oProxyCommand=touch:path", "git@-host:path", "-host:path", "ssh://-host/repo.git", "https://-host/repo.git", "-u@host:path"} {
		if _, err := RemoteHost(raw); err == nil {
			t.Fatalf("option-like backup remote %q accepted", raw)
		}
	}
}

// GL-107: hosts the OS resolver reads as IPv4 addresses (inet_aton forms)
// get the dotted-quad spelling, so they can't pass the same-host check or
// the loopback refusal as host names.
func TestRemoteHostCanonicalizesResolverIPv4Forms(t *testing.T) {
	for raw, want := range map[string]string{
		"https://16909060/y":               "1.2.3.4",
		"https://0x01020304/y":             "1.2.3.4",
		"ssh://git@1.2.3/r.git":            "1.2.0.3",
		"git@1.131844:r.git":               "1.2.3.4",
		"https://0x1.0x2.03.4/r":           "1.2.3.4",
		"https://001.002.003.004/r":        "1.2.3.4",
		"https://1.2.3.4./r":               "1.2.3.4",
		"https://backup.example/r":         "backup.example",
		"https://1e3.example/r":            "1e3.example",
		"https://08.example/r":             "08.example",
		"https://123.example.0x10/r":       "123.example.0x10",
		"https://4294967296/r":             "4294967296",
		"https://1.2.3.256/r":              "1.2.3.256",
		"https://9.9.9.9.9/r":              "9.9.9.9.9",
		"ssh://git@backup-1.example/r.git": "backup-1.example",
		"https://[::ffff:127.1.2.3]/r":     "",
		"ssh://git@0x7f000001/owner/r.git": "",
		"https://127.1/r":                  "",
		"https://2130706433/r":             "",
		"https://0x7f.0.0.1/r":             "",
		"https://0177.0.0.1/r":             "",
		"https://0/r":                      "",
		"git@0x0:owner/repo.git":           "",
	} {
		got, err := RemoteHost(raw)
		if want == "" {
			if err == nil || !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("%q: accepted as %q (%v)", raw, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Fatalf("%q: %q %v, want %q", raw, got, err, want)
		}
	}
	if err := ValidateDestination("backup", []string{"https://16909060/y"}, []string{"https://1.2.3.4/x"}); err == nil {
		t.Fatal("a decimal spelling of the origin address passed the same-host check")
	}
}
