// Package importjob orchestrates getting books onto a server: matching import
// candidates against the library's books, computing their placement, transferring
// them, and asking for a rescan. This file holds what finds the existing books a
// new book's placement anchors on: author folders and the author rule
// (Planner.siblings applies it).
package importjob

import (
	"regexp"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// AuthorFolders maps each author (normalized via normalize) to the author-folder
// spelling already used on the server for their books, so a new book reuses the
// existing folder instead of a near-duplicate - e.g. an Audible "L. A. McBride"
// placed under an existing "L.A. McBride" rather than beside it. When an author has
// more than one spelling on the server the most common wins (first-seen breaks ties,
// deterministically). Books at the library root (no author folder) and blank authors
// are skipped. Built once per plan (one pass) so the per-record lookup is O(1);
// unlike siblings it keys on author alone (any series), so standalones and
// first-in-series books reuse the folder too. Look up with authorFolders[match.Normalize(author)].
func AuthorFolders(books []link.LibraryBook) map[string]string {
	type tally struct {
		counts map[string]int
		best   string
		bestN  int
	}
	byAuthor := map[string]*tally{}
	for _, b := range books {
		seg := authorFolderSeg(b.Path)
		key := match.Normalize(Credit(b.Authors))
		if seg == "" || key == "" {
			continue
		}
		t := byAuthor[key]
		if t == nil {
			t = &tally{counts: map[string]int{}}
			byAuthor[key] = t
		}
		t.counts[seg]++
		if t.counts[seg] > t.bestN { // strictly-greater keeps the first-seen spelling on ties
			t.best, t.bestN = seg, t.counts[seg]
		}
	}
	out := make(map[string]string, len(byAuthor))
	for k, t := range byAuthor {
		out[k] = t.best
	}
	return out
}

// authorFolderSeg is the first path segment of a book rel path (its author folder),
// or "" for a root-level loose file that has no author folder.
func authorFolderSeg(rel string) string {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

// sameAuthor reports whether two author credits share a person. A credit may name
// several authors - an embedded tag reads "Shirtaloon, Travis Deverell" where the
// Audible record carries only its first author - so each side is split into names
// and any (normalized) overlap counts. Siblings also require the same series, so a
// shared co-author alone never links unrelated books.
func sameAuthor(a, b string) bool {
	names := map[string]bool{}
	for _, n := range authorNames(a) {
		names[n] = true
	}
	for _, n := range authorNames(b) {
		if names[n] {
			return true
		}
	}
	return false
}

// authorSep splits a multi-author credit: commas, semicolons, ampersands, slashes
// and a standalone "and".
var authorSep = regexp.MustCompile(`(?i)\s*(?:[,;&/]|\band\b)\s*`)

// authorNames is a credit's individual names, normalized with the shared
// match.Normalize (so the Bridge compares names as the server does), plus
// the whole credit so an unsplit equality still matches.
func authorNames(credit string) []string {
	out := []string{}
	if n := match.Normalize(credit); n != "" {
		out = append(out, n)
	}
	for _, p := range authorSep.Split(credit, -1) {
		if n := match.Normalize(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}
