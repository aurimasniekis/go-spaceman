package storetest

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// maxLoadScale caps how far a loaded machine can relax the default guard.
const maxLoadScale = 8

// loadScale is the factor by which the default write-lock bound stretches when a hold exceeded it on a machine with
// more runnable work than CPUs. With r = max(load average, runnable threads) / CPUs above 1, a thread gets about 1/r of
// a CPU, so the same transaction takes r times as long; hyper-threads and cache sharing cost about as much again, so
// the factor is 1+r. It is 1 on an idle machine and wherever the load cannot be read, which keeps the bound at the
// requirement there (ADR-0062, update 2026-11-16).
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
	if len(f) < 4 || cpus < 1 {
		return 1
	}
	load, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 1
	}
	if run, _, ok := strings.Cut(f[3], "/"); ok {
		if n, err := strconv.ParseFloat(run, 64); err == nil && n > load {
			load = n
		}
	}
	r := load / float64(cpus)
	if r <= 1 {
		return 1
	}
	return min(1+r, maxLoadScale)
}
