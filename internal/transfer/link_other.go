//go:build !linux && !darwin

package transfer

import "os"

func linkSource(_ *os.Root, _, _, _ string) (bool, error) { return false, nil }
