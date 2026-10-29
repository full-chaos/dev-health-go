//go:build linux

package workloadtoken

import (
	"os"
	"syscall"
)

// stRdonly is ST_RDONLY in statfs f_flags: the mount is read-only.
const stRdonly = 0x1

// readOnlyFile reports whether an open file is on a read-only mount (a Docker
// or Compose secret, a Kubernetes projected volume): nobody can change it
// through this mount.
func readOnlyFile(file *os.File) bool {
	var fs syscall.Statfs_t
	if err := syscall.Fstatfs(int(file.Fd()), &fs); err != nil {
		return false
	}
	return fs.Flags&stRdonly != 0
}
