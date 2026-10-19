package chapterdetect

import (
	"strconv"
	"strings"
	"testing"
)

// script builds a word list: a string is words spoken back to back (0.3 s each,
// 0.1 s apart), a float64 a pause of that many seconds before the next word.
type script struct {
	words []Word
	t     float64
}

func (s *script) say(text string) *script {
	for _, w := range strings.Fields(text) {
		s.words = append(s.words, Word{Text: " " + w, Start: s.t, End: s.t + 0.3})
		s.t += 0.4
	}
	return s
}

func (s *script) pause(sec float64) *script { s.t += sec; return s }

// prose fills sec seconds with ordinary narration.
func (s *script) prose(sec float64) *script {
	end := s.t + sec
	for s.t < end {
		s.say("and the story went on")
	}
	return s
}

func titles(chs []Chapter) []string {
	out := make([]string, len(chs))
	for i, c := range chs {
		out[i] = c.Title
	}
	return out
}

func TestPauseTest(t *testing.T) {
	s := (&script{}).say("Chapter One").prose(600)
	s.pause(0.5).say("Chapter Two").prose(600) // too short a pause: prose, not a heading
	s.pause(2).say("Chapter Three").prose(600)
	res := Detect(s.words, nil, s.t)
	if got := titles(res.Chapters); strings.Join(got, "|") != "Chapter One|Chapter Three" {
		t.Fatalf("titles = %q", got)
	}
}

func TestPauseFromSilence(t *testing.T) {
	// The words run on (whisper's timestamps bridged the gap) but ffmpeg heard a
	// 2 s silence ending just before the heading word.
	s := (&script{}).say("Chapter One").prose(600)
	at := s.t
	s.say("Chapter Two").prose(600)
	res := Detect(s.words, []Silence{{Start: at - 2.2, End: at - 0.2}}, s.t)
	if len(res.Chapters) != 2 || res.Chapters[1].Title != "Chapter Two" {
		t.Fatalf("chapters = %+v", res.Chapters)
	}
	if res.Chapters[1].Pause < 1.9 {
		t.Fatalf("pause = %v, want the silence's 2 s", res.Chapters[1].Pause)
	}
	// A silence that ends long before the word is not its pause.
	res = Detect(s.words, []Silence{{Start: at - 10, End: at - 5}}, s.t)
	if len(res.Chapters) != 1 {
		t.Fatalf("an unrelated silence made a heading: %+v", res.Chapters)
	}
}

func TestNumberedRunDropsStrays(t *testing.T) {
	s := &script{}
	for _, n := range []string{"1", "2", "seven", "3", "4"} {
		s.pause(2).say("Chapter " + n).prose(300)
	}
	res := Detect(s.words, nil, s.t)
	if got := strings.Join(titles(res.Chapters), "|"); got != "Chapter 1|Chapter 2|Chapter 3|Chapter 4" {
		t.Fatalf("titles = %s", got)
	}
}

func TestNumbersMaySkip(t *testing.T) {
	s := &script{}
	for _, n := range []string{"two", "three", "twelve", "seventeen", "thirty"} {
		s.pause(2).say("Day " + n).prose(1200)
	}
	res := Detect(s.words, nil, s.t)
	if len(res.Chapters) != 5 || !res.Confident {
		t.Fatalf("days: %+v (confident %v, %s)", titles(res.Chapters), res.Confident, res.Reason)
	}
}

func TestNumberForms(t *testing.T) {
	for _, tc := range []struct {
		say  string
		want int
	}{
		{"Chapter 12.", 12},
		{"Chapter twenty-one", 21},
		{"Chapter twenty one", 21},
		{"Chapter one hundred four", 104},
		{"Part first", 1},
		{"Book Tenth", 10},
		{"Chapter one and the night", 1},
		{"Chapter 0", 0},
		{"Chapter 1000", 0},
		{"Chapter the", 0},
		// The number ends where its phrase does, and words side by side are no number.
		{"Chapter One. Two days later", 1},
		{"Chapter One. One day", 1},
		{"Chapter Twenty. One morning", 20},
		{"Chapter Three, seven years", 3},
		{"Chapter Seven. One hundred miles", 7},
		{"Chapter one two", 1},
		{"Chapter one hundred twenty-one", 121},
		{"Chapter One Hundred and Five", 105},
		{"Chapter one hundred and the night", 100},
		{"Chapter one hundred twenty one", 121},
		{"Chapter One Hundred and Twenty One", 121},
		{"Chapter one hundred and twenty-one", 121},
		{"Chapter twenty one hundred", 21},
	} {
		s := (&script{}).say(tc.say)
		n, _ := numberAt(s.words, 1)
		if n != tc.want {
			t.Errorf("%q: number %d, want %d", tc.say, n, tc.want)
		}
	}
}

func TestDedicationIsNotAHeading(t *testing.T) {
	s := (&script{}).say("Prologue").prose(600).pause(3).say("Dedication").prose(600)
	if res := Detect(s.words, nil, s.t); len(res.Chapters) != 1 {
		t.Fatalf("dedication made a chapter: %v", titles(res.Chapters))
	}
}

func TestMergeEpilogueDay(t *testing.T) {
	s := (&script{}).say("Day 1").prose(600)
	s.pause(2).say("Epilogue.").pause(1.5).say("Day 101.").pause(1).prose(600)
	res := Detect(s.words, nil, s.t)
	if len(res.Chapters) != 2 {
		t.Fatalf("chapters = %v", titles(res.Chapters))
	}
	last := res.Chapters[1]
	if last.Title != "Epilogue. Day 101" || last.Heading != "Epilogue. Day 101." {
		t.Fatalf("merged heading = %q (heard %q)", last.Title, last.Heading)
	}
}

func TestTitles(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *script
		want string
	}{
		{"up to the gap", (&script{}).say("Stave Four. The Last of the Spirits.").pause(1).prose(10), "Stave Four. The Last of the Spirits"},
		{"lower case", (&script{}).say("stave two the first of the three spirits").pause(1).prose(10), "Stave Two The First Of The Three Spirits"},
		{"upper case", (&script{}).say("STAVE THREE THE SECOND").pause(1).prose(10), "Stave Three The Second"},
		{"mixed kept", (&script{}).say("Chapter 4: the iPhone").pause(1).prose(10), "Chapter 4: the iPhone"},
		{"no gap: heading and number", (&script{}).say("Chapter Nine").prose(10), "Chapter Nine"},
		{"never shorter than the number", (&script{}).say("Chapter").pause(1).say("Nine.").pause(1).prose(10), "Chapter Nine"},
	} {
		res := Detect(tc.s.words, nil, tc.s.t)
		if got := res.Chapters[0].Title; got != tc.want {
			t.Errorf("%s: title %q, want %q", tc.name, got, tc.want)
		}
	}
	long := (&script{}).say("Chapter 1 " + strings.Repeat("Wordy ", 9)).pause(1).prose(10)
	long.words[5].Text = " " + strings.Repeat("x", 300)
	if got := Detect(long.words, nil, long.t).Chapters[0].Title; len(got) > MaxTitleBytes {
		t.Errorf("title of %d bytes", len(got))
	}
}

func TestFrontMatterAndOpening(t *testing.T) {
	s := (&script{}).say("This is Audible. Podium Audio presents").pause(2).say("Chapter 1").prose(600)
	res := Detect(s.words, nil, s.t)
	if len(res.Chapters) != 1 || res.Chapters[0].Start != 0 || res.Chapters[0].Title != "Chapter 1" {
		t.Fatalf("front matter not folded: %+v", res.Chapters)
	}
	if res.Chapters[0].HeadingAt < 3 {
		t.Fatalf("heading_at = %v, want where Chapter 1 was said", res.Chapters[0].HeadingAt)
	}
	s = (&script{}).prose(400).pause(2).say("Chapter 1").prose(600)
	res = Detect(s.words, nil, s.t)
	if got := strings.Join(titles(res.Chapters), "|"); got != "Opening|Chapter 1" || res.Chapters[0].Spoken {
		t.Fatalf("opening: %s %+v", got, res.Chapters[0])
	}
}

func TestBoundaries(t *testing.T) {
	s := (&script{}).say("Chapter 1").prose(600).pause(2)
	at := s.t
	s.say("Chapter 2").prose(600)
	c := Detect(s.words, nil, s.t+5).Chapters
	if c[1].Start != at-boundaryLead || c[0].End != c[1].Start || c[1].End != s.t+5 {
		t.Fatalf("chapters = %+v (heading at %v)", c, at)
	}
	// Never before the end of the word before the heading: here whisper put the
	// last word's end 0.1 s before the heading, and the silence gave the pause.
	words := []Word{{" Chapter", 0, 0.3}, {" 1", 0.4, 0.7}, {" the", 100, 100.3}, {" end.", 100.4, 109.9},
		{" Chapter", 110, 110.3}, {" 2", 110.4, 110.7}, {" more", 111, 111.3}}
	c = Detect(words, []Silence{{Start: 106, End: 110}}, 200).Chapters
	if len(c) != 2 || c[1].Start != 109.9 {
		t.Fatalf("chapters = %+v, want chapter 2 from the previous word's end", c)
	}
}

func TestShortBookWithoutHeadings(t *testing.T) {
	s := (&script{}).prose(1800)
	res := Detect(s.words, nil, 1800)
	if len(res.Chapters) != 1 || !res.Confident || res.Chapters[0].Start != 0 || res.Chapters[0].End != 1800 {
		t.Fatalf("short book: %+v", res)
	}
}

func TestLowConfidence(t *testing.T) {
	none := (&script{}).prose(10)
	one := (&script{}).say("Chapter 1").prose(10)
	long := &script{}
	for i, n := range []string{"1", "2", "3", "4"} {
		long.pause(2).say("Chapter " + n).prose(1200)
		if i == 2 {
			long.prose(9600 - 1200) // chapter 3 runs 2 h 40 min
		}
	}
	gappy := &script{}
	for i := 1; i <= 30; i++ {
		if i%2 == 0 && i < 30 {
			continue // half the headings unheard
		}
		gappy.pause(2).say("Chapter " + strconv.Itoa(i)).prose(600)
	}
	for _, tc := range []struct {
		name     string
		s        *script
		duration float64
		want     string
	}{
		{"no heading", none, 7200, "no chapter headings were heard in the narration"},
		{"one heading", one, 7200, "only one chapter heading was heard in the narration"},
		{"long chapter", long, long.t, "chapter 3 runs 2 h 40 min, so headings were probably missed"},
		{"gappy run", gappy, gappy.t, "the narration numbers chapters 1 to 30 but only 16 were heard"},
	} {
		res := Detect(tc.s.words, nil, tc.duration)
		if res.Confident || res.Reason != tc.want {
			t.Errorf("%s: confident %v, reason %q, want %q", tc.name, res.Confident, res.Reason, tc.want)
		}
	}
	// A quarter missing is still confident; and day numbers skip by design.
	ok := &script{}
	for i := 1; i <= 8; i++ {
		if i == 4 || i == 6 {
			continue
		}
		ok.pause(2).say("Chapter " + strconv.Itoa(i)).prose(900)
	}
	if res := Detect(ok.words, nil, ok.t); !res.Confident {
		t.Errorf("6 of 8 heard: %s", res.Reason)
	}
}

func TestSpokenDuration(t *testing.T) {
	for sec, want := range map[float64]string{5700: "95 min", 9600: "2 h 40 min", 10800: "3 h"} {
		if got := spokenDuration(sec); got != want {
			t.Errorf("%v: %q, want %q", sec, got, want)
		}
	}
}

// TestNumberOfIsNotAHeading: a number followed by "of" names a place in something
// else ("Book 10 of The Messenger" in the end credits), never a heading.
func TestNumberOfIsNotAHeading(t *testing.T) {
	for _, tc := range []struct {
		say  string
		want int
	}{
		{"Book 10 of The Messenger.", 1},
		{"Chapter 3 of the report", 1},
		{"Book ten of the series", 1},
		{"Chapter twenty one of it", 1},
		{"Chapter 3. Of Mice and Men", 2}, // a title after a break may start with "Of"
		{"Chapter 3 Off the road", 2},
		{"Chapter Five. One of the guards", 2}, // "One" begins the prose, not the number
	} {
		s := (&script{}).say("Chapter 1").prose(600).pause(2).say(tc.say).prose(60)
		if got := len(Detect(s.words, nil, s.t).Chapters); got != tc.want {
			t.Errorf("%q: %d chapters, want %d", tc.say, got, tc.want)
		}
	}
}

// A pause a window cut splits in two (each window hears its half) is still one
// pause: halves of 1.3 s, under MinPause each, make the 2.6 s pause before a heading.
func TestPauseSplitAtAWindowCut(t *testing.T) {
	s := (&script{}).say("Chapter One").prose(600)
	cut := s.t + 1.3
	s.words[len(s.words)-1].End = cut // whisper stretched window k's last word to its end
	s.pause(2.6).say("Chapter Two").prose(600)
	halves := []Silence{{Start: cut - 1.3, End: cut}, {Start: cut, End: cut + 1.3}}
	if res := Detect(s.words, halves, s.t); len(res.Chapters) != 2 {
		t.Fatalf("the split pause lost the heading: %v", titles(res.Chapters))
	}
	// Overlapping a little (the decoders' rounding) is one pause too.
	overlap := []Silence{{Start: cut - 1.3, End: cut + 0.05}, {Start: cut, End: cut + 1.3}}
	if res := Detect(s.words, overlap, s.t); len(res.Chapters) != 2 {
		t.Fatalf("overlapping halves lost the heading: %v", titles(res.Chapters))
	}
	// Denied: two short silences far apart are no long pause.
	apart := []Silence{{Start: cut - 1.3, End: cut - 0.6}, {Start: cut + 0.6, End: cut + 1.3}}
	if res := Detect(s.words, apart, s.t); len(res.Chapters) != 1 {
		t.Fatalf("silences apart made a heading: %v", titles(res.Chapters))
	}
}

// Headings whose words start at the same time are taken in transcript order, the
// same every run (the runs are a map).
func TestEqualStartsAreDeterministic(t *testing.T) {
	words := []Word{{" The", 0, 0.3}, {" end.", 0.4, 0.8},
		{" Part", 400, 400}, {" One.", 400, 400}, {" Chapter", 400, 400}, {" One.", 400, 400.5},
		{" It", 401.5, 401.8}, {" was", 401.9, 402.2}, {" dark.", 402.3, 402.6}}
	sil := []Silence{{Start: 390, End: 399.9}}
	want := Detect(words, sil, 500)
	for range 200 {
		got := Detect(words, sil, 500)
		if len(got.Chapters) != len(want.Chapters) || got.Chapters[len(got.Chapters)-1] != want.Chapters[len(want.Chapters)-1] {
			t.Fatalf("detections differ: %+v vs %+v", got.Chapters, want.Chapters)
		}
	}
	if last := want.Chapters[len(want.Chapters)-1]; last.Title != "Part One. Chapter One" || last.Number != 1 {
		t.Fatalf("title = %q (number %d), want the heading in transcript order, chapter 1", last.Title, last.Number)
	}
}

// A chapter's number is its "chapter" heading's, whatever follows the number; a
// heading of another kind numbers something else, and an unnumbered one none.
func TestChapterNumbers(t *testing.T) {
	s := (&script{}).say("Prologue.").prose(600)
	s.pause(2).say("Chapter One.").prose(600)
	s.pause(2).say("Chapter Two, The Storm.").prose(600)
	s.pause(2).say("Chapter three: in which").prose(600)
	s.pause(2).say("Part Two.").pause(1.6).say("Chapter Four.").prose(600)
	s.pause(2).say("Epilogue.").prose(600)
	res := Detect(s.words, nil, s.t)
	var got []string
	for _, c := range res.Chapters {
		got = append(got, c.Heading+"="+strconv.Itoa(c.Number))
	}
	want := "Prologue.=0|Chapter One.=1|Chapter Two,=2|Chapter three:=3|Part Two. Chapter Four.=4|Epilogue.=0"
	if strings.Join(got, "|") != want {
		t.Fatalf("numbers = %s\nwant      %s", strings.Join(got, "|"), want)
	}
	d := (&script{}).say("Day two.").prose(600)
	d.pause(2).say("Day three.").prose(600)
	if res := Detect(d.words, nil, d.t); res.Chapters[0].Number != 0 || res.Chapters[1].Number != 0 {
		t.Fatalf("a day heading numbered a chapter: %+v", res.Chapters)
	}
}
