//go:build race

package storetest

// raceScale multiplies the default write-lock guard under the race detector, which slows a transaction several times.
const raceScale = 5
