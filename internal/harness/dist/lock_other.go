//go:build !unix

package dist

import "context"

// lockFile is a no-op where flock is unavailable: only the in-process singleflight applies.
func lockFile(context.Context, string) (func(), error) { return func() {}, nil }

func tryLockFile(string) (func(), bool, error) { return func() {}, true, nil }
