//go:build !race

package storetest

// raceScale multiplies the default write-lock guard; without the race detector the default stands.
const raceScale = 1
