//go:build !race

package auth

// raceEnabled reports whether the test binary was built with the race detector.
const raceEnabled = false
