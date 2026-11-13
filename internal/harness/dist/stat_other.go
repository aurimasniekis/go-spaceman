//go:build !linux && !darwin

package dist

import "os"

// statFingerprint is unavailable here: every launch hashes the binary again.
func statFingerprint(os.FileInfo) (fingerprint, bool) { return fingerprint{}, false }
