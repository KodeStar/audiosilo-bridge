package importjob

import (
	"cmp"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// titleRange is a range of volumes in a title ("Books 1-3", "(5–8)"). As in
// pkg/match's titleSpan, numbers embedded in a word are not volume bounds.
// publisherPack and completePack are pack names packWord does not cover.
var titleRange = regexp.MustCompile(`(?:^|[^\p{L}\p{N}])(\d{1,3}(?:\.\d+)?)\s*[-–]\s*(\d{1,3}(?:\.\d+)?)(?:[^\p{L}\p{N}]|$)`)
var publisherPack = regexp.MustCompile(`(?i)\bpublisher['’]s pack\b`)
var completePack = regexp.MustCompile(`(?i)\b(?:the )?complete (?:series|collection)\b`)

// packTitle reports whether title names a pack: a pack word, a range of
// volumes or a publisher's pack.
func packTitle(title string) bool {
	return packWord.MatchString(title) || titleRange.MatchString(title) || publisherPack.MatchString(title)
}

// bookPackTitle is the label that marks b as a pack, its title or else its
// folder name, or "" for none. A tagged title can be a track name ("End
// Credits") even when the folder names the whole box set. Only the book's own
// path segment counts.
func bookPackTitle(b link.LibraryBook) string {
	if packTitle(b.Title) {
		return b.Title
	}
	if folder := path.Base(b.Path); packTitle(folder) {
		return folder
	}
	return ""
}

// volumeRange is the first and last volume a pack spans.
type volumeRange struct {
	start, end float64
}

// readRange reads the first range pattern matches in s, its two numbers
// captured as the first and second groups.
func readRange(pattern *regexp.Regexp, s string) (volumeRange, bool) {
	m := pattern.FindStringSubmatch(s)
	if m == nil {
		return volumeRange{}, false
	}
	start, okStart := source.ParsePosition(m[1])
	end, okEnd := source.ParsePosition(m[2])
	return volumeRange{start, end}, okStart && okEnd
}

// bookRanges is the distinct ranges b's series list it at.
func bookRanges(b link.LibraryBook) []volumeRange {
	var spans []volumeRange
	for _, s := range b.Series {
		if span, ok := readRange(rangePosition, s.Position); ok && !slices.Contains(spans, span) {
			spans = append(spans, span)
		}
	}
	return spans
}

// packName is a pack's own name, so packs match on it: words such as
// "Omnibus" are common to different series by one author. A folder titled
// just "Books 1-3" takes its series name.
func packName(title, series string) string {
	title = completePack.ReplaceAllString(title, " ")
	title = publisherPack.ReplaceAllString(title, " ")
	title = packWord.ReplaceAllString(title, " ")
	title = titleRange.ReplaceAllString(title, " ")
	title = strings.Trim(title, " \t,:;-–()[]")
	if number, ok := strings.CutPrefix(strings.ToLower(title), "book "); ok {
		if _, ok := source.ParsePosition(number); ok {
			return series
		}
	}
	return cmp.Or(title, series)
}

// packCandidate is one pack candidate scored by its name: ord is its index in
// BookMatcher.cands, ranged whether the listing gives it a range (spans then
// holds those, else the range its label states, if any), index its names.
type packCandidate struct {
	ord    int
	ranged bool
	spans  []volumeRange
	index  *match.Index
}

// newPackCandidate prepares book as a pack under its title's name and, when
// it differs, its label's.
func newPackCandidate(ord int, book match.Book, label string, spans []volumeRange) packCandidate {
	ranged := len(spans) > 0
	if !ranged {
		if span, ok := readRange(titleRange, label); ok {
			spans = []volumeRange{span}
		}
	}
	book.Title = packName(book.Title, book.Series)
	names := []match.Book{book}
	if label != "" {
		if name := packName(label, book.Series); name != book.Title {
			book.Title = name
			names = append(names, book)
		}
	}
	return packCandidate{ord: ord, ranged: ranged, spans: spans, index: match.NewIndexWith(names, match.ImportOptions())}
}

// bestPackTitle admits a positionless box set to the range folders. A stated
// single position never opens a range. A stated range has already tried its
// range partition, and can still identify a pack labeled only in its title.
func (m *BookMatcher) bestPackTitle(q match.Query, span volumeRange, allowRanges bool) (int, float64, bool) {
	title := cmp.Or(q.Title, q.TitleShort)
	if span == (volumeRange{}) {
		span, _ = readRange(titleRange, title)
	}
	q.Title = packName(title, q.Series)
	q.TitleShort = q.Title

	best, high, tied := -1, 0.0, false
	for _, p := range m.packs {
		if p.ranged && !allowRanges {
			continue
		}
		if span != (volumeRange{}) && len(p.spans) > 0 && !slices.Contains(p.spans, span) {
			continue
		}
		_, score, ok := p.index.BestScore(q)
		if !ok {
			continue
		}
		switch {
		case score > high:
			best, high, tied = p.ord, score, false
		case score == high && m.owner[p.ord] != m.owner[best]:
			tied = true
		}
	}
	if best < 0 || tied {
		return -1, 0, false
	}
	return best, high, true
}

// firstRangeFallback is the display fallback for a range at its first
// volume's position. The series name alone cannot identify a pack on the
// series-named opening book, so that one is skipped for the next best.
func (m *BookMatcher) firstRangeFallback(q match.Query) (int, float64, bool) {
	seriesNamed := func(i int) bool {
		b := m.books[m.owner[i]]
		return seriesNamedTitle(b.Title, q.Series) || slices.ContainsFunc(b.Series, func(s link.SeriesRef) bool {
			return seriesNamedTitle(b.Title, s.Name)
		})
	}
	if i, score, ok := m.single.best(q); !ok || !seriesNamed(i) {
		return i, score, ok
	}
	// Only a vetoed winner needs a second choice. Preserve index order on ties.
	best, high := -1, 0.0
	for _, i := range m.single.ord {
		_, score, ok := match.BestWith(m.cands[i:i+1], q, match.ImportOptions())
		if ok && score > high && !seriesNamed(i) {
			best, high = i, score
		}
	}
	return best, high, best >= 0
}

// seriesNamedTitle recognizes the opening title with its edition or "Book 1"
// decoration, including a plain trailing 1.
func seriesNamedTitle(title, series string) bool {
	return series != "" && (bareSeriesTitle(match.CleanTitle(title, ""), series) || match.CleanTitle(title, series) == "1")
}
