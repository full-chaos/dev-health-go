//go:build unix

package workloadtoken

import (
	"os"
	"syscall"
)

// fileOwnerUID is the owner uid of an open file's FileInfo.
func fileOwnerUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}
