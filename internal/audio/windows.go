package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
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

// WindowAudioDir is where chapter_detecting keeps the window FLACs (moved there
// from chapters/ with the rest of the window layer) while it cuts the detected
// chapters from them; it removes them once the chapters are cut.
const WindowAudioDir = WindowsDir + "/" + ChaptersDir

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
	var probe func(t float64) (float64, bool, error)
	if ffmpegPath != "" {
		probe = func(t float64) (float64, bool, error) { return quietestNear(ctx, ffmpegPath, bookFile, t) }
	}
	cuts, err := windowCuts(ctx, duration, probe)
	if err != nil {
		return Manifest{}, err
	}
	chs := make([]Chapter, 0, len(cuts)-1)
	for i := 0; i+1 < len(cuts); i++ {
		chs = append(chs, Chapter{Chapter: i + 1, Start: cuts[i], End: cuts[i+1], Duration: cuts[i+1] - cuts[i]})
	}
	return Manifest{Source: bookFile, Title: title, Style: StyleWindows, Duration: duration, ChapterCount: len(chs),
		Chapters: chs}, nil
}

// windowCuts is where the windows of a file of duration seconds start, and its end:
// each cut at probe's quiet point near its target (none: at the target). A probe
// that fails (it ran out its time bound on a wedged mount, or ffmpeg can't read the
// file) ends the probing: the rest are cut at their targets, rather than a long book
// waiting out the bound once per cut.
func windowCuts(ctx context.Context, duration float64, probe func(t float64) (float64, bool, error)) ([]float64, error) {
	cuts := []float64{0}
	for pos := 0.0; duration-pos > windowMinSplit; {
		t := pos + windowTarget
		c := t
		if probe != nil {
			mid, ok, err := probe(t)
			switch {
			case err != nil && ctx.Err() != nil:
				return nil, ctx.Err()
			case err != nil:
				probe = nil
			case ok:
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
	return append(cuts, duration), nil
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
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// silenceFilter is silencedetect at the spike's settings. asWindow first makes the
// audio what a window FLAC holds (16 kHz mono), so the silences split hears while
// cutting a window are the ones its FLAC would give.
func silenceFilter(asWindow bool) string {
	f := fmt.Sprintf("silencedetect=noise=%s:d=%s", silenceNoise, strconv.FormatFloat(silenceMinSec, 'f', -1, 64))
	if asWindow {
		f = "aformat=sample_rates=16000:channel_layouts=mono," + f
	}
	return f
}

// The silencedetect log's times: an ffmpeg before 7 prints a tiny one with an
// exponent ("6.25e-05").
var (
	reSilenceStart = regexp.MustCompile(`silence_start: (-?[0-9.]+(?:[eE][-+]?[0-9]+)?)`)
	reSilenceEnd   = regexp.MustCompile(`silence_end: (-?[0-9.]+(?:[eE][-+]?[0-9]+)?)`)
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
	args = append(args, "-i", file, "-vn", "-af", silenceFilter(false), "-f", "null", "-")
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

// windowSilences is a window's silences as kept beside its FLAC
// (chNNN.silences.json): on the file's timeline, with the window's span, so a file
// a different cut of the book left is never taken for this window's.
type windowSilences struct {
	Start    float64   `json:"start"`
	End      float64   `json:"end"`
	Silences []Silence `json:"silences"`
}

// windowSilencesPath is window w's silences file in dir (the dir of its FLAC).
func windowSilencesPath(dir string, w int) string {
	return filepath.Join(dir, fmt.Sprintf("ch%03d.silences.json", w))
}

// writeWindowSilences keeps window w's silences beside its FLAC in dir.
func writeWindowSilences(dir string, w Chapter, sil []Silence) error {
	if sil == nil {
		sil = []Silence{}
	}
	raw, err := json.Marshal(windowSilences{Start: w.Start, End: w.End, Silences: sil})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(windowSilencesPath(dir, w.Chapter), append(raw, '\n'), 0o644)
}

// readWindowSilences reads window w's kept silences, ok only for this very window.
func readWindowSilences(dir string, w Chapter) ([]Silence, bool) {
	raw, err := os.ReadFile(windowSilencesPath(dir, w.Chapter)) //nolint:gosec // the book's work dir
	if err != nil {
		return nil, false
	}
	var ws windowSilences
	if json.Unmarshal(raw, &ws) != nil || ws.Silences == nil || ws.Start != w.Start || ws.End != w.End {
		return nil, false
	}
	return ws.Silences, true
}

// windowFLAC is window w's FLAC in dir, ok when it is there whole.
func windowFLAC(dir string, w Chapter) (string, bool) {
	flac := filepath.Join(dir, ChapterFileName(w.Chapter))
	return flac, complete(flac, w.Duration)
}

// WindowHeard reports whether WindowSilences can give window w's silences from
// dir without the source: kept there, or its FLAC there whole to listen to.
func WindowHeard(dir string, w Chapter) bool {
	if _, ok := readWindowSilences(dir, w); ok {
		return true
	}
	_, ok := windowFLAC(dir, w)
	return ok
}

// WindowSilences is window w's silences on the file's timeline: the ones split
// heard while it cut the window's FLAC in dir, else heard now from that FLAC
// (bounded by timeout) and kept beside it the same way, so a later look (a Retry
// after the chapters weren't found) decodes nothing.
func WindowSilences(ctx context.Context, ffmpegPath, dir string, w Chapter, timeout time.Duration) ([]Silence, error) {
	if sil, ok := readWindowSilences(dir, w); ok {
		return sil, nil
	}
	flac, ok := windowFLAC(dir, w)
	if !ok {
		return nil, fmt.Errorf("window %d FLAC missing (%s); split must run first", w.Chapter, flac)
	}
	sil, err := DetectSilences(ctx, ffmpegPath, flac, 0, w.Duration, timeout)
	if err != nil {
		return nil, err
	}
	for i := range sil {
		sil[i].Start += w.Start
		sil[i].End += w.Start
	}
	return sil, writeWindowSilences(dir, w, sil)
}

// windowPart is the stretch of one window FLAC a chapter takes: from start, for
// dur (seconds into the window).
type windowPart struct {
	file       string
	start, dur float64
}

// minWindowPart is the shortest stretch of a window a chapter takes as a part: a
// chapter boundary on a window's edge leaves less of the next one by rounding.
const minWindowPart = 0.001

// CutFromWindows cuts each chapter of m (the detected manifest of the file the
// windows were cut from) out of the window FLACs in windowDir into
// workDir/chapters, as Split cuts from the source: a chapter inside one window is a
// seek in it, one across window edges joins its parts (FLAC decodes sample
// accurately, so the chapter is the same audio). Resumable like Split, a complete
// FLAC skipped. A chapter whose windows aren't all there is left for Split (from
// the source): left counts them.
func CutFromWindows(ctx context.Context, m Manifest, windows []Chapter, windowDir, workDir, ffmpegPath string) (left int, err error) {
	if ffmpegPath == "" {
		return 0, errors.New("ffmpeg unavailable; cannot cut chapters")
	}
	chaptersDir := filepath.Join(workDir, ChaptersDir)
	if err := os.MkdirAll(chaptersDir, 0o750); err != nil {
		return 0, err
	}
	for _, ch := range m.Chapters {
		if err := ctx.Err(); err != nil {
			return left, err
		}
		out := filepath.Join(chaptersDir, ChapterFileName(ch.Chapter))
		if complete(out, ch.Duration) {
			continue
		}
		parts, ok := windowParts(ch, windows, windowDir)
		if !ok {
			left++
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, splitChapterBound(ch.Duration))
		err := cutParts(cctx, ffmpegPath, parts, out, fmt.Sprintf("chapter %d", ch.Chapter))
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return left, ctx.Err()
			}
			return left, err
		}
	}
	return left, nil
}

// windowParts is the stretches of the windows (FLACs in dir) chapter ch takes, in
// order; ok is false when one of them isn't there.
func windowParts(ch Chapter, windows []Chapter, dir string) ([]windowPart, bool) {
	var parts []windowPart
	for _, w := range windows {
		from, to := math.Max(ch.Start, w.Start), math.Min(ch.End, w.End)
		if to-from < minWindowPart {
			continue
		}
		f := filepath.Join(dir, ChapterFileName(w.Chapter))
		if !complete(f, w.Duration) {
			return nil, false
		}
		parts = append(parts, windowPart{file: f, start: from - w.Start, dur: to - from})
	}
	return parts, len(parts) > 0
}

// SpanPart is a stretch of a FLAC: start and length in seconds.
type SpanPart struct {
	File       string
	Start, Dur float64
}

// CutSpan encodes parts, joined in order, as one mono/16 kHz FLAC at out (the
// read-along heal's clip across the chapter FLACs of one file, Phase 3c).
func CutSpan(ctx context.Context, ffmpegPath string, parts []SpanPart, out string) error {
	if ffmpegPath == "" {
		return errors.New("ffmpeg unavailable; cannot cut a clip")
	}
	if len(parts) == 0 {
		return errors.New("no audio to cut")
	}
	wp := make([]windowPart, len(parts))
	for i, p := range parts {
		wp[i] = windowPart{file: p.File, start: p.Start, dur: p.Dur}
	}
	return cutParts(ctx, ffmpegPath, wp, out, "clip")
}

// cutParts encodes parts, joined in order, as one mono/16 kHz FLAC at out (one
// part is a plain seek, as CutClip cuts; several are trimmed inputs joined by the
// concat filter, every input's -ss/-t exact on a FLAC).
func cutParts(ctx context.Context, ffmpegPath string, parts []windowPart, out, label string) error {
	if len(parts) == 1 {
		p := parts[0]
		return encodeFLAC(ctx, ffmpegPath, []string{"-ss", ftoa(p.start), "-i", p.file, "-t", ftoa(p.dur)}, out, label)
	}
	var in []string
	var graph strings.Builder
	for i, p := range parts {
		in = append(in, "-ss", ftoa(p.start), "-t", ftoa(p.dur), "-i", p.file)
		fmt.Fprintf(&graph, "[%d:a]", i)
	}
	fmt.Fprintf(&graph, "concat=n=%d:v=0:a=1[a]", len(parts))
	tail := append([]string{"-filter_complex", graph.String(), "-map", "[a]"}, flacOutputArgs...)
	return encode(ctx, ffmpegPath, in, tail, out, label, nil)
}
