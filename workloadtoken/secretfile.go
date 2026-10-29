package workloadtoken

import "os"

// Seams over the file facts the secret-file rule reads; tests replace them.
var (
	ownerOf         = fileOwnerUID
	onReadOnlyMount = readOnlyFile
	processUID      = os.Geteuid
	// ownersReported is whether this system reports file owners (unix); where
	// it does not, the owner rule does not apply and the mode rule alone
	// decides, as in v0.31.0.
	ownersReported = ownersReportedHere
)
