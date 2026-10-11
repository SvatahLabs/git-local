//go:build !windows

package keymaterial

// Mode bits make a key owner-private outside Windows; see Load.
func protectKey(string) error  { return nil }
func checkKeyACL(string) error { return nil }
