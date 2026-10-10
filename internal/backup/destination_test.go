package backup

import "testing"

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
