package main

import (
	"io"
	"strings"
	"testing"
)

// A command after serve's flags is refused rather than starting the Bridge.
func TestServeRefusesACommandAfterItsFlags(t *testing.T) {
	err := run([]string{"--data", t.TempDir(), "reset-password"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "put a command before its flags") {
		t.Fatalf("err = %v", err)
	}
}
