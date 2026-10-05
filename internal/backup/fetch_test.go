package backup

import "testing"

func TestBackupRefValidationRejectsRevisionAndRefspecInjection(t *testing.T) {
	good := "refs/gitlocal/backups/123e4567-e89b-42d3-a456-426614174000/20260926T123456Z-123e4567-e89b-42d3-a456-426614174001"
	if err := ValidateBackupRef(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"origin/main", "refs/heads/main", good + ":refs/heads/main", good + "^{}", "refs/gitlocal/backups/../../main", "refs/gitlocal/backups/123e4567-e89b-42d3-a456-426614174000/20261326T123456Z-123e4567-e89b-42d3-a456-426614174001"} {
		if err := ValidateBackupRef(bad); err == nil {
			t.Fatalf("invalid backup ref %q accepted", bad)
		}
	}
}
