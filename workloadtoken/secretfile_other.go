//go:build !linux

package workloadtoken

import "os"

// readOnlyFile cannot tell a read-only mount here: it reports false, so only
// the owner rule admits a file.
func readOnlyFile(*os.File) bool { return false }
