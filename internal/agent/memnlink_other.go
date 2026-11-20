//go:build !unix

package agent

import "io/fs"

func fileIdentity(fs.FileInfo) (uint64, int64) { return 0, 0 }

func linkCount(fs.FileInfo) uint64 { return 1 }
