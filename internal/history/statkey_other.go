//go:build !darwin && !linux

package history

import "io/fs"

// Without inode numbers and change times a stat cache can't tell a replaced
// file from the cached one, so every file is read.
const statKeysSupported = false

func statKeyOf(fs.FileInfo) (statKey, bool) { return statKey{}, false }
