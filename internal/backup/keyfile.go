package backup

import "gitlocal/internal/keymaterial"

// CreateKeyFile writes a new raw key with owner-only permissions. It refuses
// existing files and paths within the checkout.
func CreateKeyFile(path, repoRoot string) error {
	return keymaterial.Create(path, repoRoot)
}

// LoadKeyFile accepts only an owner-private raw key outside the checkout.
func LoadKeyFile(path, repoRoot string) ([]byte, error) {
	return keymaterial.Load(path, repoRoot)
}
