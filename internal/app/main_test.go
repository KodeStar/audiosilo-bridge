package app

import (
	"os"
	"testing"
)

// TestMain keeps the engine the tests start offline: with no ffmpeg on the
// machine, its first start would otherwise download one.
func TestMain(m *testing.M) {
	_ = os.Setenv("AUDIOSILO_BRIDGE_TOOLS_AUTO_DOWNLOAD", "false")
	os.Exit(m.Run())
}
