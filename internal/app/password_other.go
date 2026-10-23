//go:build !unix

package app

// checkOwner has nothing to check where there is no root user.
func checkOwner(string) error { return nil }
