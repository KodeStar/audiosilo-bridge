package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/chapterdetect"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// owJSON renders an openai-whisper raw transcript from segments of "word@start"
// tokens (window-relative seconds; each word 0.3 s long).
func owJSON(segs ...string) []byte {
	doc := transcript.Transcript{Language: "en"}
	for i, seg := range segs {
		s := transcript.Segment{ID: i}
		var text strings.Builder
		for _, tok := range strings.Fields(seg) {
			w, at, _ := strings.Cut(tok, "@")
			var t float64
			_, _ = fmt.Sscanf(at, "%g", &t)
			w = strings.ReplaceAll(w, "_", " ")
			s.Words = append(s.Words, transcript.Word{W: " " + w, Start: t, End: t + 0.3})
			text.WriteString(" " + w)
		}
		s.Start, s.End, s.Text = s.Words[0].Start, s.Words[len(s.Words)-1].End, text.String()
		doc.Segments = append(doc.Segments, s)
	}
	raw, _ := transcript.EncodeOpenAI(doc)
	return raw
}

func TestLoadWindowTimelineGluesTokensAndOffsets(t *testing.T) {
	// whisper.cpp writes sub-word tokens; a token without a leading space continues
	// the word before it.
	wc := `{"transcription":[{"offsets":{"from":0,"to":2000},"text":" Chapter one","tokens":[
		{"text":" Chap","offsets":{"from":0,"to":300}},{"text":"ter","offsets":{"from":300,"to":600}},
		{"text":" one","offsets":{"from":700,"to":900}},{"text":".","offsets":{"from":900,"to":950}}]},
		{"offsets":{"from":3000,"to":4000},"text":" no words","tokens":[]}]}`
	windows := []audio.Chapter{{Chapter: 1, Start: 0, End: 50}, {Chapter: 2, Start: 50, End: 100}}
	tl, err := loadWindowTimeline(windows, func(w audio.Chapter) ([]byte, error) {
		if w.Chapter == 1 {
			return owJSON("Hello@1"), nil
		}
		return []byte(wc), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	words := tl.words()
	got := make([]string, len(words))
	for i, w := range words {
		got[i] = fmt.Sprintf("%s@%g", strings.TrimSpace(w.Text), w.Start)
	}
	if want := "Hello@1|Chapter@50|one.@50.7|no words@53"; strings.Join(got, "|") != want {
		t.Fatalf("words = %s, want %s", strings.Join(got, "|"), want)
	}
	if words[1].End != 50.6 || words[2].End != 50.95 {
		t.Fatalf("glued ends = %v, %v", words[1].End, words[2].End)
	}
}

func TestSplitTimelineAtBoundaries(t *testing.T) {
	windows := []audio.Chapter{{Chapter: 1, Start: 0, End: 100}, {Chapter: 2, Start: 100, End: 200}}
	tl, err := loadWindowTimeline(windows, func(w audio.Chapter) ([]byte, error) {
		if w.Chapter == 1 {
			// The second segment carries the end of chapter 1 and the start of chapter 2.
			return owJSON("Chapter@0 One@0.4 the@1", "end.@59 Chapter@62 Two@62.4"), nil
		}
		return owJSON("more@10"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	docs := tl.split([]audio.Chapter{{Chapter: 1, Start: 0, End: 61.8}, {Chapter: 2, Start: 61.8, End: 200}})
	if len(docs[0].Segments) != 2 || len(docs[1].Segments) != 2 {
		t.Fatalf("segments = %d / %d, want 2 / 2", len(docs[0].Segments), len(docs[1].Segments))
	}
	tail, head := docs[0].Segments[1], docs[1].Segments[0]
	if strings.TrimSpace(tail.Text) != "end." || strings.TrimSpace(head.Text) != "Chapter Two" {
		t.Fatalf("split texts = %q / %q", tail.Text, head.Text)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !near(head.Start, 0.2) || head.Words[0].Start != head.Start || !near(docs[1].Segments[1].Start, 48.2) {
		t.Fatalf("chapter 2 times not relative to its start: %+v", docs[1].Segments)
	}
	if head.ID != 0 || docs[1].Segments[1].ID != 1 || docs[0].Language != "en" {
		t.Fatalf("ids / language = %d %d %q", head.ID, docs[1].Segments[1].ID, docs[0].Language)
	}
	// The synthesized raw reads as a complete openai-whisper transcript.
	raw, _ := transcript.EncodeOpenAI(docs[1])
	if !transcript.Complete(raw) {
		t.Fatal("synthesized raw is not a complete transcript")
	}
	if tr, err := transcript.Normalize(raw, transcript.Meta{Chapter: 2}); err != nil || len(tr.Segments) != 2 {
		t.Fatalf("normalize: %v %+v", err, tr)
	}
}

// TestGroupByPosition: the player's list is grouped by logical position, so a
// front-matter Prologue shares chapter 1's entry and an Epilogue past the last
// numbered chapter stays its own entry after it.
func TestGroupByPosition(t *testing.T) {
	long := strings.Repeat("words ", 200)
	texts := []string{"Prologue. " + long, "Chapter One. " + long, "Chapter Two. " + long, "Chapter Three. " + long,
		"Epilogue. " + long}
	titles := []string{"Prologue", "Chapter One", "Chapter Two", "Chapter Three", "Epilogue"}
	var m audio.Manifest
	var tl windowTimeline
	for i, text := range texts {
		start := float64(i) * 600
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: i + 1, Title: titles[i], Start: start, End: start + 600,
			Duration: 600})
		tl.segments = append(tl.segments, timedSegment{start: start + 1, end: start + 590, text: " " + text,
			words: []timedWord{{text: " x", start: start + 1, end: start + 2}}})
	}
	m.Duration = 3000
	got := groupByPosition(m, tl.split(m.Chapters), nil)
	var desc []string
	for _, e := range got {
		desc = append(desc, fmt.Sprintf("%s@%d+%d", e.Title, e.StartMs, e.LengthMs))
	}
	want := "Prologue / Chapter One@0+1200000|Chapter Two@1200000+600000|Chapter Three@1800000+600000|Epilogue@2400000+600000"
	if strings.Join(desc, "|") != want {
		t.Fatalf("entries = %s\nwant      %s", strings.Join(desc, "|"), want)
	}
}

// chapterlessFixture lays out a work dir as the asr stage leaves a windowed book:
// a 20 s source, a windows manifest of two 10 s windows, their FLACs and raws.
func chapterlessFixture(t *testing.T, ffmpeg string, duration float64, raws ...[]byte) store.Book {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "book.m4a")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
		"sine=frequency=220:duration=20", "-c:a", "aac", src).CombinedOutput(); err != nil {
		t.Fatalf("generate source: %v: %s", err, out)
	}
	work := filepath.Join(dir, "work")
	m := audio.Manifest{Source: src, Style: audio.StyleWindows, Duration: duration, ChapterCount: 2, Chapters: []audio.Chapter{
		{Chapter: 1, Start: 0, End: 10, Duration: 10}, {Chapter: 2, Start: 10, End: 20, Duration: 10}}}
	if err := os.MkdirAll(filepath.Join(work, transcript.RawDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatal(err)
	}
	if err := audio.Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatal(err)
	}
	for i, raw := range raws {
		p := filepath.Join(work, transcript.RawDir, transcript.RawName(i+1))
		if err := os.WriteFile(p, raw, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeASRProvenance(work, asrProvenance{Backend: "fake", Model: "m", Language: "en"}); err != nil {
		t.Fatal(err)
	}
	return store.Book{ID: 1, SourcePath: src, WorkDir: work}
}

func TestChapterDetectStage(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	book := chapterlessFixture(t, ffmpeg, 20,
		owJSON("Chapter@0.5 One@0.9 it_began@2.2", "and@4 on@4.5"),
		owJSON("Chapter@2.5 Two@2.9 it_ended@4.2"))
	e := NewExecutor(Config{FFmpeg: ffmpeg})
	// fromSource says whether a run split chapters from the source (its note).
	run := func() (fromSource bool) {
		t.Helper()
		_, err := e.Execute(context.Background(), book, state.ChapterDetecting, scheduler.StageReport{Note: func(msg string) {
			fromSource = fromSource || strings.Contains(msg, "from the source")
		}})
		if err != nil {
			t.Fatalf("chapter_detecting: %v", err)
		}
		return fromSource
	}
	// The chapters are cut from the window FLACs, which hold the whole book already.
	if run() {
		t.Fatal("the chapters were split from the source, not cut from the windows")
	}
	m, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Style != audio.StyleMarkers || !m.Detected || m.ChapterCount != 2 || m.Chapters[0].Title != "Chapter One" ||
		m.Chapters[1].Start != 12.3 || m.Chapters[1].End != 20 {
		t.Fatalf("detected manifest = %+v", m)
	}
	// The window layer moved aside; the raws are the chapters' own now, frozen.
	for _, p := range []string{filepath.Join(audio.WindowsDir, audio.ManifestName), filepath.Join(audio.WindowsDir, windowsDetectionName),
		filepath.Join(audio.WindowsDir, transcript.RawDir, transcript.RawName(1))} {
		if _, err := os.Stat(filepath.Join(book.WorkDir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	raw2 := filepath.Join(book.WorkDir, transcript.RawDir, transcript.RawName(2))
	info, err := os.Stat(raw2)
	if err != nil || info.Mode().Perm() != 0o444 || !rawComplete(raw2) {
		t.Fatalf("chapter 2 raw: %v %v", info, err)
	}
	b, _ := os.ReadFile(raw2) //nolint:gosec // test
	if !strings.Contains(string(b), "Chapter Two") || strings.Contains(string(b), "began") {
		t.Fatalf("chapter 2 raw holds the wrong words: %s", b)
	}
	fp, _ := manifestFingerprint(book.WorkDir)
	if readASRProvenance(book.WorkDir).ManifestSHA != fp {
		t.Fatal("asr.json doesn't carry the detected manifest's fingerprint")
	}
	for ch := 1; ch <= 2; ch++ {
		if !audioComplete(filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(ch))) {
			t.Errorf("chapter %d not split", ch)
		}
	}
	if _, err := os.Stat(filepath.Join(book.WorkDir, audio.WindowAudioDir)); !os.IsNotExist(err) {
		t.Errorf("the window FLACs outlived the cut: %v", err)
	}
	list, err := ReadDetectedChapters(book.WorkDir)
	if err != nil || len(list.Chapters) != 2 || list.Chapters[0].StartMs != 0 || list.Chapters[1].StartMs != 12300 ||
		list.Chapters[1].LengthMs != 7700 || !list.CurrentFor(book.WorkDir) {
		t.Fatalf("chapters_detected.json = %+v, %v", list, err)
	}
	if !scheduler.SentinelExists(book.WorkDir, string(state.ChapterDetecting)) {
		t.Fatal("no sentinel")
	}
	// A crash after the move: the stage runs again from windows/ (the window FLACs
	// are gone, so the missing chapter is split from the source) and lands on the
	// same book.
	_ = os.Remove(scheduler.SentinelPath(book.WorkDir, string(state.ChapterDetecting)))
	_ = os.Remove(DetectedChaptersPath(book.WorkDir))
	_ = os.Remove(filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(2)))
	if !run() {
		t.Fatal("the missing chapter wasn't split from the source")
	}
	again, err := ReadDetectedChapters(book.WorkDir)
	if err != nil || again.ManifestSHA != list.ManifestSHA || len(again.Chapters) != 2 {
		t.Fatalf("after the re-run: %+v, %v", again, err)
	}
	if !audioComplete(filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(2))) {
		t.Error("chapter 2 not split again from the source")
	}
}

func audioComplete(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Size() > 0
}

func TestChapterDetectStageParksWhenNotConfident(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	// The manifest says two hours (the confidence checks apply) and nothing is
	// announced.
	book := chapterlessFixture(t, ffmpeg, 7200, owJSON("and@1 so@2 on@3"), owJSON("and@1 more@2"))
	_, err = NewExecutor(Config{FFmpeg: ffmpeg}).Execute(context.Background(), book, state.ChapterDetecting, scheduler.StageReport{})
	var park *scheduler.ParkError
	if !errors.As(err, &park) || park.Code != state.ParkChaptersNotDetected ||
		park.Reason != ChaptersNotDetectedPrefix+": no chapter headings were heard in the narration" {
		t.Fatalf("err = %v, want a chapters_not_detected park", err)
	}
	// Nothing moved: a Retry runs the detection again.
	if m, _ := audio.ReadManifest(book.WorkDir); m.Style != audio.StyleWindows {
		t.Fatalf("manifest = %q", m.Style)
	}
	if _, err := os.Stat(filepath.Join(book.WorkDir, audio.WindowsDir)); !os.IsNotExist(err) {
		t.Fatalf("windows/ written on a park: %v", err)
	}
	// The silences heard are kept whatever the verdict: a Retry decodes nothing (the
	// window FLACs gone, it parks the same way rather than failing for them).
	for w := 1; w <= 2; w++ {
		_ = os.Remove(filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(w)))
	}
	_, err = NewExecutor(Config{FFmpeg: ffmpeg}).Execute(context.Background(), book, state.ChapterDetecting, scheduler.StageReport{})
	if !errors.As(err, &park) || park.Code != state.ParkChaptersNotDetected {
		t.Fatalf("retry: err = %v, want the same park without decoding", err)
	}
}

func TestChapterDetectStageRefusesAChapteredManifest(t *testing.T) {
	work := t.TempDir()
	if err := audio.WriteManifest(work, audio.Manifest{Style: audio.StyleMarkers, Chapters: []audio.Chapter{{Chapter: 1}}}); err != nil {
		t.Fatal(err)
	}
	_, err := NewExecutor(Config{FFmpeg: "ffmpeg"}).Execute(context.Background(), store.Book{WorkDir: work},
		state.ChapterDetecting, scheduler.StageReport{})
	if err == nil || !strings.Contains(err.Error(), "not a windows manifest") {
		t.Fatalf("err = %v", err)
	}
}

// detectedFixture is a detected manifest of chapters announcing titles (each with
// prose after it) and the timeline that says them.
func detectedFixture(titles []string) (audio.Manifest, windowTimeline) {
	long := strings.Repeat("words ", 200)
	m := audio.Manifest{Style: audio.StyleMarkers, Detected: true}
	var tl windowTimeline
	for i, title := range titles {
		start := float64(i) * 600
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: i + 1, Title: title, Start: start, End: start + 600, Duration: 600})
		tl.segments = append(tl.segments, timedSegment{start: start + 1, end: start + 590, text: " " + title + ". " + long,
			words: []timedWord{{text: " x", start: start + 1, end: start + 2}}})
	}
	m.Duration = float64(len(titles)) * 600
	m.ChapterCount = len(m.Chapters)
	return m, tl
}

// The classification a detected list is grouped by puts every part at its entry's
// ordinal, where the narration's numbering skips one (a heading not heard) too:
// entry k is position k, and the last entry the last position.
func TestDetectedPositionsAreTheListsOrdinals(t *testing.T) {
	m, tl := detectedFixture([]string{"Chapter One", "Chapter Two", "Chapter Three", "Chapter Five", "Chapter Six", "Chapter Seven"})
	class, list := detectedClassification(m, tl.split(m.Chapters), nil)
	if len(list) != 6 || class.LogicalCount != 6 {
		t.Fatalf("%d entries, logical %d", len(list), class.LogicalCount)
	}
	for k, e := range list {
		for _, ch := range e.Parts {
			if pos, ok := class.PositionOf(ch); !ok || pos != k+1 {
				t.Fatalf("entry %d's chapter %d at position %d (%v)", k+1, ch, pos, ok)
			}
		}
	}
	// Numbering that doesn't skip keeps the narration's positions as they are.
	m2, tl2 := detectedFixture([]string{"Prologue", "Chapter One", "Chapter Two", "Chapter Three"})
	class2, list2 := detectedClassification(m2, tl2.split(m2.Chapters), nil)
	edge, _ := edgeChapters(m2.Chapters, func(ch audio.Chapter) (string, error) {
		return clipBytes(transcript.PlainText(tl2.split(m2.Chapters)[ch.Chapter-1]), chapterOpeningBytes), nil
	}, func(audio.Chapter) (int, bool, error) { return 200, true, nil })
	if raw := classifyEdgeChapters(edge); fmt.Sprint(raw.Positions) != fmt.Sprint(class2.Positions) || len(list2) != 3 {
		t.Fatalf("an unskipped numbering was renumbered: %v vs %v (%d entries)", raw.Positions, class2.Positions, len(list2))
	}
}

// A numbered chapter whose heading runs on into its title ("Chapter Four, The
// Storm.") is its own entry at its own position: the classifier can't read that
// opening as an announcement, so it takes the number the detector heard.
func TestDetectedTitledHeadingKeepsItsEntry(t *testing.T) {
	m, tl := detectedFixture([]string{"Chapter One", "Chapter Two", "Chapter Three", "Chapter Four, The Storm", "Chapter Five", "Chapter Six"})
	docs := tl.split(m.Chapters)
	// Without the heard numbers the opening reads as an unnumbered section, which
	// joins the next chapter's entry (the fixture does exercise the case).
	if _, list := detectedClassification(m, docs, nil); len(list) != 5 {
		t.Fatalf("control: %d entries, want chapter 4 joined with 5", len(list))
	}
	class, list := detectedClassification(m, docs, []int{1, 2, 3, 4, 5, 6})
	if len(list) != 6 || class.LogicalCount != 6 {
		t.Fatalf("%d entries, logical %d, want 6", len(list), class.LogicalCount)
	}
	for k, e := range list {
		if pos, ok := class.PositionOf(k + 1); len(e.Parts) != 1 || e.Parts[0] != k+1 || !ok || pos != k+1 {
			t.Fatalf("entry %d = %+v, chapter %d at position %d", k+1, e, k+1, pos)
		}
	}
	if list[3].Title != "Chapter Four, The Storm" {
		t.Fatalf("entry 4 title = %q", list[3].Title)
	}
}

// The stages after chapter_detecting classify a detected book with the
// classification its list was grouped by, whatever the text layer says by then
// (QA re-transcribed an opening): the sidecars' position k stays the list's entry k.
func TestDetectedBookKeepsItsClassification(t *testing.T) {
	work := t.TempDir()
	m, tl := detectedFixture([]string{"Opening", "Chapter One", "Chapter two, in which the storm broke over the hills", "Chapter Three", "Chapter Four"})
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatal(err)
	}
	fp, err := manifestFingerprint(work)
	if err != nil {
		t.Fatal(err)
	}
	book := store.Book{WorkDir: work}
	docs := tl.split(m.Chapters)
	list, err := writeDetectedChapters(book, m, docs, nil, fp)
	if err != nil {
		t.Fatal(err)
	}
	// The text layer later reads chapter 3's heading as an announcement.
	for i, d := range docs {
		text := transcript.PlainText(d)
		if i == 2 {
			text = "Chapter Two. The storm broke over the hills. " + text
		}
		if err := os.MkdirAll(filepath.Join(work, transcript.TextDir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, transcript.TextDir, transcript.TextName(i+1)), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aligned := func(class edgeClassification) bool {
		for k, e := range list.Chapters {
			for _, ch := range e.Parts {
				if pos, _ := class.PositionOf(ch); pos != k+1 && (k != 0 || pos != 0) {
					return false
				}
			}
		}
		return true
	}
	class, err := classifyBookEdges(book)
	if err != nil || !aligned(class) {
		t.Fatalf("classification %+v (%v) is off the list's ordinals", class.Positions, err)
	}
	// The control: classified again from the text layer, the positions would move.
	if err := os.Remove(filepath.Join(work, detectedPositionsName)); err != nil {
		t.Fatal(err)
	}
	if again, err := classifyBookEdges(book); err != nil || aligned(again) {
		t.Fatalf("the text layer's classification %+v (%v) agrees anyway: the test proves nothing", again.Positions, err)
	}
}

// Nothing a window's transcript times past the window's own audio reaches the
// timeline (a hallucinated "!" there sorted between the next window's "Day" and
// "85." lost the heading); glued tokens keep their lowest probability; a segment
// whisper gave no words stays wordless in the synthesized raw.
func TestWindowTimelineStaysInItsWindow(t *testing.T) {
	low, high := 0.3, 0.9
	w1 := transcript.Transcript{Language: "en", Segments: []transcript.Segment{
		{ID: 0, Start: 1, End: 2, Text: " Owalyn", Words: []transcript.Word{{W: " Ow", Start: 1, End: 1.3, P: &high}, {W: "alyn", Start: 1.3, End: 1.6, P: &low}}},
		{ID: 1, Start: 3, End: 5, Text: " Thank you for listening.", Words: []transcript.Word{}},
		{ID: 2, Start: 10.4, End: 10.8, Text: " !", Words: []transcript.Word{{W: " !", Start: 10.4, End: 10.8}}},
	}}
	w2 := transcript.Transcript{Language: "en", Segments: []transcript.Segment{
		{ID: 0, Start: 0.2, End: 1.2, Text: " Day 85.", Words: []transcript.Word{{W: " Day", Start: 0.2, End: 0.5}, {W: " 85.", Start: 0.6, End: 1.0}}},
	}}
	windows := []audio.Chapter{{Chapter: 1, Start: 0, End: 10, Duration: 10}, {Chapter: 2, Start: 10, End: 20, Duration: 10}}
	tl, err := loadWindowTimeline(windows, func(w audio.Chapter) ([]byte, error) {
		if w.Chapter == 1 {
			return transcript.EncodeOpenAI(w1)
		}
		return transcript.EncodeOpenAI(w2)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range tl.words() {
		got = append(got, strings.TrimSpace(w.Text))
	}
	if strings.Join(got, "|") != "Owalyn|Thank you for listening.|Day|85." {
		t.Fatalf("words = %v", got)
	}
	if p := tl.segments[0].words[0].p; p == nil || *p != low {
		t.Fatalf("glued probability = %v, want the lowest", p)
	}
	docs := tl.split([]audio.Chapter{{Chapter: 1, Start: 0, End: 20, Duration: 20}})
	for _, s := range docs[0].Segments {
		if strings.Contains(s.Text, "Thank you") && len(s.Words) != 0 {
			t.Fatalf("a wordless segment got words: %+v", s.Words)
		}
	}
}

// DetectionRerunnable says a book stopped at chapter_detecting can run the stage
// again from what it kept (a generate job continues it): the windows transcribed,
// their silences to be had or the detection kept for them, the file the one
// inspected (its size, and not written since the probe); or the layer moved aside
// with its detection. Anything short of that is denied.
func TestDetectionRerunnable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "book.m4b")
	mustWriteFile := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The audio file, last written well before inspect probed it (at mod).
	writeSource := func(body string, mod time.Time) {
		t.Helper()
		mustWriteFile(src, body)
		if err := os.Chtimes(src, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().Add(-time.Hour)
	writeSource("the audio", before)
	work := filepath.Join(dir, "work")
	m := audio.Manifest{Source: src, Style: audio.StyleWindows, Duration: 20, ChapterCount: 2, Chapters: []audio.Chapter{
		{Chapter: 1, Start: 0, End: 10, Duration: 10}, {Chapter: 2, Start: 10, End: 20, Duration: 10}}}
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	writeManifestStruct(t, work, m)
	probe := `{"format":{"duration":"20","size":"9"}}`
	writeProbeJSON(t, work, probe)
	for w := 1; w <= 2; w++ {
		mustWriteFile(filepath.Join(work, transcript.RawDir, transcript.RawName(w)), string(owJSON("and@1 on@2")))
		mustWriteFile(filepath.Join(work, audio.ChaptersDir, audio.ChapterFileName(w)), strings.Repeat("f", 512))
	}
	if !DetectionRerunnable(work) {
		t.Fatal("a complete window layer isn't rerunnable")
	}
	flac := filepath.Join(work, audio.ChaptersDir, audio.ChapterFileName(2))
	raw := filepath.Join(work, transcript.RawDir, transcript.RawName(2))
	for _, tc := range []struct {
		denied         string
		spoil, restore func()
	}{
		{"the file replaced since it was inspected",
			func() { writeSource("another encode", before) }, func() { writeSource("the audio", before) }},
		{"the file edited in place since it was inspected, its size kept (markers written into the tag's padding)",
			func() { writeSource("the AUDIO", time.Now().Add(time.Hour)) }, func() { writeSource("the audio", before) }},
		{"a probe that states no size (unknown is no match)",
			func() { writeProbeJSON(t, work, `{"format":{"duration":"20"}}`) }, func() { writeProbeJSON(t, work, probe) }},
		{"a window's audio gone with no silences kept (a purge)",
			func() { _ = os.Remove(flac) }, func() { mustWriteFile(flac, strings.Repeat("f", 512)) }},
		{"a window's transcript missing",
			func() { _ = os.Remove(raw) }, func() { mustWriteFile(raw, string(owJSON("and@1 on@2"))) }},
	} {
		tc.spoil()
		if DetectionRerunnable(work) {
			t.Fatalf("rerunnable with %s", tc.denied)
		}
		tc.restore()
		if !DetectionRerunnable(work) {
			t.Fatalf("not rerunnable once %s was undone", tc.denied)
		}
	}
	// A file dated a little after the probe (a library on a clock running ahead of
	// the Bridge's, the file copied in just before inspect) is still the one read.
	writeSource("the audio", time.Now().Add(sourceClockSlack/2))
	if !DetectionRerunnable(work) {
		t.Fatal("a file dated within the clock slack of its probe isn't rerunnable")
	}
	writeSource("the audio", before)
	// A window's audio gone with no silences kept, but the confident detection kept
	// for the windows (the stage stopped between keeping it and the move): the
	// stage takes it as it is and cuts from the source what the windows can't give.
	wdir := filepath.Join(work, audio.WindowsDir)
	detection := filepath.Join(wdir, windowsDetectionName)
	kept := windowDetection{WindowsSHA: windowsSHA(m), Result: chapterdetect.Result{Confident: true}}
	_ = os.Remove(flac)
	writeJSONFile(t, detection, kept)
	if !DetectionRerunnable(work) {
		t.Fatal("a layer in place with its confident detection kept isn't rerunnable")
	}
	_ = os.Remove(detection)
	mustWriteFile(flac, strings.Repeat("f", 512))
	// The layer moved aside: rerunnable only with the detection it was moved by.
	writeManifestStruct(t, wdir, m)
	if err := os.Rename(filepath.Join(work, transcript.RawDir), filepath.Join(wdir, transcript.RawDir)); err != nil {
		t.Fatal(err)
	}
	if DetectionRerunnable(work) {
		t.Fatal("rerunnable after a move with no detection kept")
	}
	writeJSONFile(t, detection, kept)
	if !DetectionRerunnable(work) {
		t.Fatal("a moved layer with its detection isn't rerunnable")
	}
}
