//go:build unix

package workloadtoken

import (
	"os"
	"syscall"
)

// ownersReportedHere: unix reports file owners.
const ownersReportedHere = true

// openSecretFile opens path for reading without blocking: a FIFO (or another
// special file) opens at once and is then refused as not regular, instead of
// waiting for a writer. Reads of a regular file are not affected.
func openSecretFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// fileOwnerUID is the owner uid of an open file's FileInfo.
func fileOwnerUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}
