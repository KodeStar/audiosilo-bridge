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

// The native downloader and the image script both use the public Bridge release.
func TestWhisperReleaseURLs(t *testing.T) {
	const base = "https://github.com/KodeStar/audiosilo-bridge/releases/download"
	const asset = "whisper-cli-linux-amd64-cpu.tar.gz"
	release := base + "/" + WhisperCLIReleaseTag
	if got := whisperAssetURL(WhisperCLIReleaseTag, asset); got != release+"/"+asset {
		t.Errorf("archive URL = %q", got)
	}
	if got := whisperChecksumsURL(WhisperCLIReleaseTag); got != release+"/checksums.txt" {
		t.Errorf("manifest URL = %q", got)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docker", "fetch-whisper.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `base="`+base+`/$1"`) {
		t.Errorf("fetch-whisper.sh must download from %s", base)
	}
}
