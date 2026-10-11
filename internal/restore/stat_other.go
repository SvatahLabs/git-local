//go:build !darwin && !linux

package restore

import (
	"io/fs"
	"os"
)

// sameFile compares file identity; when it can't be established, the image
// is hashed again.
func sameFile(a, b fs.FileInfo) bool { return os.SameFile(a, b) }
