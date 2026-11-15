//go:build !unix

package dist

import (
	"context"
	"os"
)

// lockFile is a no-op where flock is unavailable: only the in-process singleflight applies.
func lockFile(context.Context, string) (func(), error) { return func() {}, nil }

func tryLockFile(string) (func(), bool, error) { return func() {}, true, nil }

// ownedByMe can't check ownership here: the mode checks apply alone.
func ownedByMe(os.FileInfo) bool { return true }
