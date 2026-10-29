//go:build !unix

package workloadtoken

import "os"

// ownersReportedHere: this system does not report file owners.
const ownersReportedHere = false

// fileOwnerUID cannot tell the owner here; the owner rule does not apply
// (ownersReportedHere is false) and the mode rule alone decides.
func fileOwnerUID(os.FileInfo) (uint32, bool) { return 0, false }

// openSecretFile opens path for reading.
func openSecretFile(path string) (*os.File, error) { return os.Open(path) }
