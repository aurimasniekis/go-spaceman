package storetest

// ScaleFromLoadavgForTest exposes scaleFromLoadavg to the external test package.
func ScaleFromLoadavgForTest(s string, cpus int) float64 { return scaleFromLoadavg(s, cpus) }
