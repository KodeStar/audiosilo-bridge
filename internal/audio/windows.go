package audio

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// StyleWindows: one audio file with no usable chapter markers, cut into long
// duration WINDOWS for transcription (BRIDGE-PLAN.md Phase 1d). The windows are not
// chapters: the pipeline's chapter_detecting stage finds the chapters in the
// narration and replaces this manifest with a detected one (StyleMarkers,
// Detected). Split cuts a windows manifest exactly like a markers one, seeking in
// the single file.
const StyleWindows = "windows"

// WindowsDir is where chapter_detecting moves the window layer aside once the
// chapters are found: the windows manifest, the window raw transcripts and the
// detection. Transcript text, so the transcript retention removes it.
const WindowsDir = "windows"

// The window sizes, measured in spike 4 (BRIDGE-PLAN.md, Phase 1b): every segment
// pays 5 to 7 s of model load on a worker, so windows are long. A window ends at
// the middle of the longest silence within windowSlack of its target (never in a
// word); a file under windowMinSplit is one window, and a last window under
// windowMinLast joins the one before it.
const (
	windowTarget   = 1500.0
	windowSlack    = 120.0
	windowMinLast  = 300.0
	windowMinSplit = windowTarget + windowMinLast
	// fullCoverage: one marker covering this share of the file is a title, not a
	// chapter table (some encoders write the book's own title as one chapter).
	fullCoverage = 0.95
)

// silenceProbe is how a cut point is looked for: ffmpeg's silencedetect at the
// spike's settings, over windowSlack either side of the target, time-bounded so a
// wedged mount can't hang the inspect.
var (
	silenceProbeTimeout = 2 * time.Minute
	silenceNoise        = "-40dB"
	silenceMinSec       = 0.4
)

// markerless reports whether a single file's marker table is no chapter table at
// all: none, or one marker covering (nearly) the whole file.
func markerless(markers []Marker, duration float64) bool {
	switch len(markers) {
	case 0:
		return true
	case 1:
		return duration > 0 && markers[0].End-markers[0].Start >= fullCoverage*duration
	}
	return false
}

// windowsManifest cuts bookFile (duration seconds) into transcription windows. With
// ffmpeg each cut sits in the longest silence near its target (one short probe per
// cut); without it, or when a probe hears none, at the target itself.
func windowsManifest(ctx context.Context, bookFile, title string, duration float64, ffmpegPath string) (Manifest, error) {
	cuts := []float64{0}
	for pos := 0.0; duration-pos > windowMinSplit; {
		t := pos + windowTarget
		c := t
		if ffmpegPath != "" {
			mid, ok, err := quietestNear(ctx, ffmpegPath, bookFile, t)
			if err != nil && ctx.Err() != nil {
				return Manifest{}, ctx.Err()
			}
			if ok {
				c = mid
			}
		}
		// A cut late in its slack must not leave a sliver: the rest joins this window.
		if duration-c < windowMinLast {
			break
		}
		cuts = append(cuts, c)
		pos = c
	}
	cuts = append(cuts, duration)
	chs := make([]Chapter, 0, len(cuts)-1)
	for i := 0; i+1 < len(cuts); i++ {
		chs = append(chs, Chapter{Chapter: i + 1, Start: cuts[i], End: cuts[i+1], Duration: cuts[i+1] - cuts[i]})
	}
	return Manifest{Source: bookFile, Title: title, Style: StyleWindows, Duration: duration, ChapterCount: len(chs),
		Chapters: chs}, nil
}

// quietestNear returns the middle of the longest silence whose middle lies within
// windowSlack of t, and whether there is one.
func quietestNear(ctx context.Context, ffmpegPath, file string, t float64) (float64, bool, error) {
	from := math.Max(0, t-windowSlack)
	sil, err := DetectSilences(ctx, ffmpegPath, file, from, t+windowSlack-from, silenceProbeTimeout)
	if err != nil {
		return 0, false, err
	}
	best, found := 0.0, false
	var longest float64
	for _, s := range sil {
		mid := (s.Start + s.End) / 2
		if math.Abs(mid-t) > windowSlack {
			continue
		}
		if d := s.End - s.Start; !found || d > longest {
			best, longest, found = mid, d, true
		}
	}
	return best, found, nil
}

// Silence is one run of silence on the file's timeline (seconds).
type Silence struct {
	Start, End float64
}

var (
	reSilenceStart = regexp.MustCompile(`silence_start: (-?[0-9.]+)`)
	reSilenceEnd   = regexp.MustCompile(`silence_end: (-?[0-9.]+)`)
)

// DetectSilences runs ffmpeg's silencedetect (silenceNoise, silenceMinSec) over
// [from, from+dur) of file (the whole file when dur <= 0) and returns the
// silences on the FILE's timeline: ffmpeg reports times relative to the seek, so
// from is added back. A silence still open at the end of the read ends there. The
// run is bounded by timeout (0 = none beyond ctx).
func DetectSilences(ctx context.Context, ffmpegPath, file string, from, dur float64, timeout time.Duration) ([]Silence, error) {
	if ffmpegPath == "" {
		return nil, fmt.Errorf("ffmpeg unavailable; cannot listen for silences")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	args := []string{"-hide_banner", "-nostats"}
	if from > 0 {
		args = append(args, "-ss", ftoa(from))
	}
	if dur > 0 {
		args = append(args, "-t", ftoa(dur))
	}
	args = append(args, "-i", file, "-vn", "-af",
		fmt.Sprintf("silencedetect=noise=%s:d=%s", silenceNoise, strconv.FormatFloat(silenceMinSec, 'f', -1, 64)),
		"-f", "null", "-")
	cmd := exec.CommandContext(ctx, ffmpegPath, args...) //nolint:gosec // ffmpegPath is operator-resolved; file is the book's audio or a work-dir FLAC
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("silence detection exceeded its %s time bound", timeout)
		}
		return nil, fmt.Errorf("ffmpeg silencedetect: %w", err)
	}
	return parseSilences(stderr.Bytes(), from, dur), nil
}

// parseSilences reads silencedetect's log lines, offset by from. A silence still
// open at the end ends at from+dur (or its own start, with no known end).
func parseSilences(log []byte, from, dur float64) []Silence {
	var out []Silence
	open, start := false, 0.0
	for _, line := range bytes.Split(log, []byte("\n")) {
		if m := reSilenceStart.FindSubmatch(line); m != nil {
			start, open = math.Max(0, parseFloat(string(m[1])))+from, true
			continue
		}
		if m := reSilenceEnd.FindSubmatch(line); m != nil && open {
			out = append(out, Silence{Start: start, End: parseFloat(string(m[1])) + from})
			open = false
		}
	}
	if open && dur > 0 {
		out = append(out, Silence{Start: start, End: from + dur})
	}
	return out
}
