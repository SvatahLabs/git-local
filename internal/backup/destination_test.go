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
