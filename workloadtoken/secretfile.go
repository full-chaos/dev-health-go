package workloadtoken

import "os"

// Seams over the file facts the secret-file rule reads; tests replace them.
var (
	ownerOf         = fileOwnerUID
	onReadOnlyMount = readOnlyFile
	processUID      = os.Geteuid
)
