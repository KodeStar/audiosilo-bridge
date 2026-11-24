// Package source holds Record, the normalized metadata of one importable
// audiobook (an Audible title), which matching and placement read.
package source

import (
	"strconv"
	"strings"
)

// Record is normalized metadata for one importable audiobook file.
type Record struct {
	FileName   string // destination file name
	Title      string
	TitleShort string
	Author     string
	// Authors is every author credit (Author is the first); nil when the
	// source knows only one.
	Authors []string
	Series  string
	// SeriesPosition is the series position as the source spells it ("2.5",
	// "1-3"); Sequence is it parsed when it is one number.
	SeriesPosition string
	Narrator       string
	ASIN           string
	Year           string
	// ReleaseDate is the release day (YYYY-MM-DD) when the source gives a whole
	// date, else "" (a preorder's day is when it comes out).
	ReleaseDate string
	Sequence    float64
	HasSequence bool
	// RuntimeSec is the book's total length in seconds (0 = unknown). Fetch sources
	// that know it (Audible) populate it; it lets the stats sync tell an at-the-end
	// position from a mid-book one.
	RuntimeSec float64
	// UnavailableReason, when non-empty, says why this record can't be acquired
	// (e.g. an Audible periodical or a title without download rights). File-based
	// sources leave it empty; audible.import fails such a title with the reason
	// instead of asking for a license.
	UnavailableReason string
	// Preorder: the record is unavailable because it isn't released yet (its
	// UnavailableReason says so in words).
	Preorder bool
}

// ParsePosition reads a single numeric series position from a listing or a record.
func ParsePosition(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
