package toolfetch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The images fetch the whisper-cli release this code pins: a bump of
// WhisperCLIReleaseTag without the Dockerfile's WHISPER_TAG (or the reverse)
// would ship images with another whisper-cli than the native builds.
func TestDockerfilePinsTheSameWhisperTag(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "ARG WHISPER_TAG=" + WhisperCLIReleaseTag + "\n"; !strings.Contains(string(raw), want) {
		t.Fatalf("the Dockerfile doesn't pin %q (toolfetch.WhisperCLIReleaseTag)", want)
	}
}
