package storetest

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// maxLoadScale caps how far a loaded machine can relax the default guard.
const maxLoadScale = 8

// loadScale is the factor by which the write-lock bound stretches when a hold exceeded it on a machine whose one-minute
// load average is above its CPU count. With r = load / CPUs above 1, a thread gets about 1/r of a CPU, so the same
// transaction takes r times as long; hyper-threads and cache sharing cost about as much again, so the factor is 1+r. It
// is 1 on an idle machine and wherever the load cannot be read. Only the smoothed load average is used, not the
// instantaneous run queue: the run queue counts this test binary's own parallel goroutines, so a heavy-parallel run
// would loosen its own guard (ADR-0062, update 2026-11-16).
func loadScale() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 1
	}
	return scaleFromLoadavg(string(b), runtime.NumCPU())
}

// scaleFromLoadavg computes loadScale from the text of /proc/loadavg ("1.5 2.0 2.1 7/812 9001") and a CPU count.
func scaleFromLoadavg(s string, cpus int) float64 {
	f := strings.Fields(s)
	if len(f) < 1 || cpus < 1 {
		return 1
	}
	load, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 1
	}
	r := load / float64(cpus)
	if r <= 1 {
		return 1
	}
	return min(1+r, maxLoadScale)
}
