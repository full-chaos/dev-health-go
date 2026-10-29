//go:build !unix

package workloadtoken

import "os"

// fileOwnerUID cannot tell the owner here: the file is refused unless it is
// on a read-only mount.
func fileOwnerUID(os.FileInfo) (uint32, bool) { return 0, false }
