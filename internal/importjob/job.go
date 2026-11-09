package importjob

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/placement"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// PlannedItem is one import candidate's computed destination. Whether the
// library already holds it is the caller's question (BookMatcher).
type PlannedItem struct {
	RelPath string // suggested destination (folder + filename), library-relative
	// Kind is how the book is placed: beside its series' books in the library
	// (SeriesExisting), as the first of its series there (SeriesNew), or on its
	// own (Standalone); Siblings is how many of its series' books the library
	// has.
	Kind     placement.Kind
	Siblings int
}

// PlanOptions configures Plan.
type PlanOptions struct {
	Mode     placement.Mode
	Template string // used when Mode == placement.ModeTemplate
	// Books is the library's books (the server's paged listing, read whole). Plan
	// finds siblings and author folders locally in it; nil is an empty library.
	Books []link.LibraryBook
}

// Plan computes a destination for each record against a library's books
// (opts.Books). For series books the existing siblings anchor the placement so
// it mirrors the established folder/shortcode convention.
func Plan(recs []source.Record, opts PlanOptions) ([]PlannedItem, error) {
	items := make([]PlannedItem, 0, len(recs))
	p := NewPlanner(opts.Books)
	for _, rec := range recs {
		item, err := p.Plan(rec, opts.Mode, opts.Template)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// Planner is Plan over one library's books for many records: the author
// folders (AuthorFolders) are found once, and the books indexed by series, so
// a record's siblings (SiblingsFromBooks, the same books in the same order)
// cost its series' books only. Not safe for concurrent use.
type Planner struct {
	books         []link.LibraryBook
	authorFolders map[string]string
	bySeries      map[string][]int // match.NormalizeSeries(name) -> books in it, in order
}

// NewPlanner indexes books (nil is an empty library).
func NewPlanner(books []link.LibraryBook) *Planner {
	p := &Planner{books: books, authorFolders: AuthorFolders(books), bySeries: map[string][]int{}}
	for i, b := range books {
		for _, s := range b.Series {
			k := match.NormalizeSeries(s.Name)
			if at := p.bySeries[k]; len(at) == 0 || at[len(at)-1] != i {
				p.bySeries[k] = append(at, i)
			}
		}
	}
	return p
}

// siblings is SiblingsFromBooks(p.books, author, series) through the index.
func (p *Planner) siblings(author, series string) []link.LibraryBook {
	if strings.TrimSpace(series) == "" {
		return nil
	}
	var out []link.LibraryBook
	for _, i := range p.bySeries[match.NormalizeSeries(series)] {
		if b := p.books[i]; sameAuthor(Credit(b.Authors), author) {
			out = append(out, b)
		}
	}
	return out
}

// Plan is rec's destination (mode and template as PlanOptions'). A template
// that can't place it is the error, and the item still carries rec's Kind and
// Siblings.
func (p *Planner) Plan(rec source.Record, mode placement.Mode, template string) (PlannedItem, error) {
	var siblings []placement.Sibling
	for _, b := range p.siblings(rec.Author, rec.Series) {
		idx, _ := SeriesPosition(b, rec.Series)
		siblings = append(siblings, placement.Sibling{RelPath: b.Path, SeriesIndex: idx})
	}
	// Name by the clean book title - strip the series prefix and "(Book N)" /
	// "(Unabridged)" fluff so a series book lands as "SHORTCODE## - The Ugly
	// Truth", not "... - Diary of a Wimpy Kid: The Ugly Truth (Book 5)".
	clean := match.CleanTitle(rec.Title, seriesAsTitled(rec.Title, rec.Series))
	in := placement.Input{
		Author:       rec.Author,
		Title:        clean,
		TitleShort:   clean,
		SeriesName:   rec.Series,
		Sequence:     rec.Sequence,
		HasSequence:  rec.HasSequence,
		Filename:     rec.FileName,
		ASIN:         rec.ASIN,
		Narrator:     rec.Narrator,
		Year:         rec.Year,
		Siblings:     siblings,
		AuthorFolder: p.authorFolders[match.Normalize(rec.Author)],
	}
	// placement's own rule, so a template that fails still says how the book
	// would have been placed.
	item := PlannedItem{Kind: placement.KindFor(in), Siblings: len(siblings)}
	sug, err := placement.SuggestWith(mode, template, in)
	if err != nil {
		return item, err
	}
	item.RelPath, item.Kind = path.Join(sug.RelPath, sug.Filename), sug.Kind
	return item, nil
}

// seriesAsTitled is the series name as the title opens with it, with the
// article before it ("The Orchard Cycle: Books 1-3" in series "Orchard Cycle"
// is "The Orchard Cycle"), so CleanTitle removes the article and the series
// together; given the bare name it leaves the article and the separator
// behind ("The : Books 1-3"). Any other title gets series as it is.
func seriesAsTitled(title, series string) string {
	t, s := strings.TrimSpace(title), strings.TrimSpace(series)
	if s == "" {
		return series
	}
	for _, art := range []string{"the ", "a ", "an "} {
		n := len(art) + len(s)
		if len(t) < n || !strings.EqualFold(t[:len(art)], art) || !strings.EqualFold(t[len(art):n], s) {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(t[n:]); n < len(t) && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			continue // the series name is the start of a longer word
		}
		return t[:n]
	}
	return series
}

// BookMatcher matches records against a fixed server-book list via the shared
// pkg/match matcher under match.ImportOptions. It prepares stable candidate
// partitions once and reuses them for every title in a poll. A book in several
// series is a candidate under each. A book whose position is a range is matched
// by a range title only (see MatchScored). Not safe for concurrent use.
type BookMatcher struct {
	books  []link.LibraryBook
	cands  []match.Book
	owner  []int // cands[i] is a candidate for books[owner[i]]
	single matchPartition
	ranges map[float64]matchPartition // spanning candidates by their start position
}

// matchPartition keeps the original candidate indexes beside a reusable index.
type matchPartition struct {
	index *match.Index
	ord   []int
}

func (p matchPartition) best(q match.Query) (int, float64, bool) {
	if p.index != nil {
		if i, score, ok := p.index.BestScore(q); ok {
			return p.ord[i], score, true
		}
	}
	return -1, 0, false
}

// NewBookMatcher builds a matcher over an already-fetched library book list.
func NewBookMatcher(books []link.LibraryBook) *BookMatcher {
	m := &BookMatcher{books: books, ranges: map[float64]matchPartition{}}
	var single []int
	ranges := map[float64][]int{}
	for i, b := range books {
		base := match.Book{Title: b.Title, Author: Credit(b.Authors), ASIN: b.ASIN, Narrators: b.Narrators}
		spans := slices.ContainsFunc(b.Series, func(s link.SeriesRef) bool {
			_, ok := rangeStart(s.Position)
			return ok
		})
		add := func(c match.Book) {
			j := len(m.cands)
			m.cands = append(m.cands, c)
			m.owner = append(m.owner, i)
			if spans {
				ranges[c.SeriesIndex] = append(ranges[c.SeriesIndex], j)
			} else {
				single = append(single, j)
			}
		}
		if len(b.Series) == 0 {
			add(base)
			continue
		}
		for _, s := range b.Series {
			c := base
			c.Series = s.Name
			var ok bool
			if c.SeriesIndex, ok = parsePosition(s.Position); !ok {
				// A range is placed at its start, as a range title is.
				c.SeriesIndex, _ = rangeStart(s.Position)
			}
			add(c)
		}
	}
	prepare := func(ord []int) matchPartition {
		cands := make([]match.Book, len(ord))
		for i, j := range ord {
			cands[i] = m.cands[j]
		}
		return matchPartition{index: match.NewIndexWith(cands, match.ImportOptions()), ord: ord}
	}
	m.single = prepare(single)
	for start, ord := range ranges {
		m.ranges[start] = prepare(ord)
	}
	return m
}

// Match finds the server book corresponding to rec (ASIN exact first, then author
// + series + series-stripped title-token overlap, sequence parsed from the title
// when the server has no position). The matcher is shared with the server so both
// sides identify the same book identically.
func (m *BookMatcher) Match(rec source.Record) (link.LibraryBook, bool) {
	b, _, ok := m.MatchScored(rec)
	return b, ok
}

// MatchScored is Match with the match's score (match.ScoreASIN for an ASIN
// match), so a caller can compare two records that claimed one book (KeepBest).
func (m *BookMatcher) MatchScored(rec source.Record) (link.LibraryBook, float64, bool) {
	q := match.Query{
		Title: rec.Title, TitleShort: rec.TitleShort, Author: rec.Author,
		Series: rec.Series, Sequence: rec.Sequence, HasSequence: rec.HasSequence, ASIN: rec.ASIN,
	}
	// A range position ("5-6": a pack of two books) is placed at its start, so a
	// number in the title ("Pack 3") never stands in for the position the store
	// gave.
	isRange := false
	if !q.HasSequence {
		q.Sequence, isRange = rangeStart(rec.SeriesPosition)
		q.HasSequence = isRange
	}
	if rec.Narrator != "" {
		q.Narrators = []string{rec.Narrator}
	}
	i, score, ok := m.best(q, isRange)
	if !ok {
		return link.LibraryBook{}, 0, false
	}
	return m.books[m.owner[i]], score, true
}

// rangePosition is a series position spanning books: "5-6", "0.5-4", "1 - 3".
var rangePosition = regexp.MustCompile(`^\s*(\d+(?:\.\d+)?)\s*[-\x{2013}]\s*\d+(?:\.\d+)?\s*$`)

// rangeStart is a range position's first number.
func rangeStart(pos string) (float64, bool) {
	sm := rangePosition.FindStringSubmatch(pos)
	if sm == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(sm[1], 64)
	return v, err == nil
}

// best tries ASINs over every candidate before scoring the stable partitions.
// The index applies the person gate and preserves candidate order for ties.
// A range title first tries spanning books starting at its position, then
// single volumes. A single volume never tries a spanning book.
func (m *BookMatcher) best(q match.Query, isRange bool) (int, float64, bool) {
	if q.ASIN != "" {
		for i := range m.cands {
			if m.cands[i].ASIN != "" && strings.EqualFold(q.ASIN, m.cands[i].ASIN) {
				return i, match.ScoreASIN, true
			}
		}
		q.ASIN = "" // no candidate has it: spare each partition's index the same scan
	}
	if isRange {
		if i, score, ok := m.ranges[q.Sequence].best(q); ok {
			return i, score, true
		}
	}
	return m.single.best(q)
}

// Claim is one record's match: the book's path, the match's score, the
// record's work (WorkKey) and whether the record's series position agrees or
// disagrees with the book's (PositionAgrees).
type Claim struct {
	Path      string
	Score     float64
	Work      string
	Agrees    bool
	Disagrees bool
}

// PositionAgrees compares rec's series position with b's: agrees when both are
// one number and the same, disagrees when both are one number and differ, and
// neither when either has none (a range is none: a pack is matched by
// MatchScored's rules). rec's is its SeriesPosition; b's is its position in
// rec's series, else, when b is not in that series, its main series' (the
// listing puts a folder code's number there: "PG02 - ..." is 2 whatever the
// series is called).
func PositionAgrees(rec source.Record, b link.LibraryBook) (agrees, disagrees bool) {
	want, ok := parsePosition(rec.SeriesPosition)
	if !ok || len(b.Series) == 0 {
		return false, false
	}
	pos := b.Series[0].Position
	target := match.NormalizeSeries(rec.Series)
	for _, s := range b.Series {
		if target != "" && match.NormalizeSeries(s.Name) == target {
			pos = s.Position
			break
		}
	}
	got, ok := parsePosition(pos)
	if !ok {
		return false, false
	}
	return got == want, got != want
}

// WorkKey identifies the work a record is: its title (folded), its authors
// (folded, in any order) and its series position. Two records with one key
// are the same book owned twice (two ASINs, an edition each), which share a
// book without contesting it; the position keeps apart the volumes a series
// lists under one bare title. A title that folds to nothing is no identity:
// its key is "", which KeepBest never takes for the same work. Nor is a title
// with no position that is its series' bare name (bridge#43): Audible lists
// several volumes so, and with no number to tell them apart they would all
// read as one work and all keep a book only one of them is. The parsed
// sequence stands in for a position Audible gave only as a number.
func WorkKey(rec source.Record) string {
	title := match.Fold(rec.Title)
	if title == "" {
		return ""
	}
	pos := strings.TrimSpace(rec.SeriesPosition)
	if pos == "" && rec.HasSequence {
		pos = strconv.FormatFloat(rec.Sequence, 'f', -1, 64)
	}
	if pos == "" && rec.Series != "" && match.Fold(rec.Series) == title {
		return ""
	}
	authors := rec.Authors
	if len(authors) == 0 && rec.Author != "" {
		authors = []string{rec.Author}
	}
	folded := make([]string, 0, len(authors))
	for _, a := range authors {
		if f := match.Fold(a); f != "" {
			folded = append(folded, f)
		}
	}
	slices.Sort(folded)
	return title + "\x00" + strings.Join(slices.Compact(folded), "\x00") + "\x00" + pos
}

// Twins are the paths of books the library holds more than once: another
// book with the same title, authors and series (folded, in any order). A
// title matched to one of them by its words alone can't tell which copy it
// is, so its ASIN goes on neither (bridge#40). A title that folds to nothing
// has no twin.
func Twins(books []link.LibraryBook) map[string]bool {
	byKey := map[string][]string{}
	for _, b := range books {
		title := match.Fold(b.Title)
		if title == "" {
			continue
		}
		parts := make([]string, 0, len(b.Authors)+len(b.Series))
		for _, a := range b.Authors {
			if f := match.Fold(a); f != "" {
				parts = append(parts, "a\x00"+f)
			}
		}
		for _, s := range b.Series {
			parts = append(parts, "s\x00"+match.NormalizeSeries(s.Name)+"\x00"+strings.TrimSpace(s.Position))
		}
		slices.Sort(parts)
		k := title + "\x00" + strings.Join(slices.Compact(parts), "\x00")
		byKey[k] = append(byKey[k], b.Path)
	}
	out := map[string]bool{}
	for _, paths := range byKey {
		if len(paths) > 1 {
			for _, p := range paths {
				out[p] = true
			}
		}
	}
	return out
}

// SpanMismatch reports whether rec spans books (its position is a range: a
// pack) and b does not, as when a pack's title fell back to the folder at its
// first volume's position. Such a match may be shown, but the pack's ASIN is
// not the book's: an enrichment would stamp it onto what may be one volume.
func SpanMismatch(rec source.Record, b link.LibraryBook) bool {
	if _, ok := rangeStart(rec.SeriesPosition); !ok || rec.HasSequence {
		return false
	}
	return !slices.ContainsFunc(b.Series, func(s link.SeriesRef) bool {
		_, ok := rangeStart(s.Position)
		return ok
	})
}

// KeepBest settles books several records claimed. Of the claims on one path:
//   - when the best is no ASIN match (match.ScoreASIN) and some claims' series
//     position agrees with the book's (Claim.Agrees), only those compete: the
//     number tells the volumes of a series apart, even against a higher score
//     (the title words of two volumes often match one folder equally);
//   - the single best score keeps its match, and a tie for the best keeps none
//     (the matcher can't tell which is the book, so none is called "on this
//     server"), unless every tied claim is the same work (WorkKey: one book
//     owned twice), which all keep it: dropping it would download it again.
//
// A claim of the same work as the one that keeps a book keeps it too, whatever
// its score (the edition the book's ASIN names scores higher than the other).
// A path one record claimed keeps it. keep[i] reports whether claims[i]
// stands; contested[i] whether it lost a book it may well be (another title
// kept it, or none could be told apart), which is every losing claim but one
// whose position disagrees with the book's: that title is another volume, so
// it is plainly not here. An empty Path is no claim.
func KeepBest(claims []Claim) (keep, contested []bool) {
	byPath := map[string][]int{}
	for i, c := range claims {
		if c.Path != "" {
			byPath[c.Path] = append(byPath[c.Path], i)
		}
	}
	keep, contested = make([]bool, len(claims)), make([]bool, len(claims))
	for _, at := range byPath {
		w := keeper(claims, at)
		for _, i := range at {
			c := claims[i]
			keep[i] = w == i || (w >= 0 && c.Work != "" && c.Work == claims[w].Work)
			contested[i] = !keep[i] && !c.Disagrees
		}
	}
	return keep, contested
}

// keeper is the claim of at (claims on one path) that keeps the book, or -1
// when a tie between two works keeps none (KeepBest).
func keeper(claims []Claim, at []int) int {
	best := func(at []int) int {
		b := at[0]
		for _, i := range at[1:] {
			if claims[i].Score > claims[b].Score {
				b = i
			}
		}
		return b
	}
	if claims[best(at)].Score < match.ScoreASIN {
		var agree []int
		for _, i := range at {
			if claims[i].Agrees {
				agree = append(agree, i)
			}
		}
		if len(agree) > 0 {
			at = agree
		}
	}
	b := best(at)
	for _, i := range at {
		if i != b && claims[i].Score == claims[b].Score && (claims[i].Work == "" || claims[i].Work != claims[b].Work) {
			return -1 // a tie between two works: none keeps it
		}
	}
	return b
}

// MatchInBooks is a one-shot match over books. For repeated matching against the
// same library, build a BookMatcher once and reuse it.
func MatchInBooks(books []link.LibraryBook, rec source.Record) (link.LibraryBook, bool) {
	return NewBookMatcher(books).Match(rec)
}

// Credit is a book's author credits as one string ("A, B"), the shape the
// manager's server listing carried and pkg/match's author gate compares.
func Credit(authors []string) string {
	return strings.Join(authors, ", ")
}

// SeriesPosition is b's position in series (matched tolerantly, as siblings are),
// and whether b is in it with a usable number.
func SeriesPosition(b link.LibraryBook, series string) (float64, bool) {
	target := match.NormalizeSeries(series)
	for _, s := range b.Series {
		if match.NormalizeSeries(s.Name) == target {
			return parsePosition(s.Position)
		}
	}
	return 0, false
}

// parsePosition reads a server series position ("3", "2.5"); anything else, and
// "", is no position.
func parsePosition(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
