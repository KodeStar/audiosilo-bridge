package librivoxjob

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-bridge/internal/importjob"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/placement"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// maxName bounds a book folder's or a section's own name (bytes, before the
// "NN - " prefix, the extension, ".part" or the " (LibriVox <id>)" suffix), so
// every name stays inside the 255 bytes a filesystem allows.
const maxName = 180

// variousAfter: a work with more authors than this is placed under Various.
const variousAfter = 3

// Various is the author folder of a work with more than variousAfter authors.
const Various = "Various"

// segment is s as one path segment: control characters and runs of spaces as
// one space, placement's Sanitize (no "/", "\\" or NUL, ":" as " -", no
// leading or trailing dot or space), cut to maxName bytes on a rune boundary
// and trimmed again. "" when nothing is left ("..", "   ").
func segment(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	s = placement.Sanitize(s)
	if len(s) > maxName {
		cut := maxName
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = placement.Sanitize(s[:cut])
	}
	return s
}

// bookNames is the book's author and title as placement takes them: the title
// is the work's, or the project's own when the work has more than one LibriVox
// recording (each its own folder: "Metamorphosis (version 2)"), either when the
// other is empty, else "LibriVox <id>"; the author is the first credit, Various
// over three. work is nil when meta has no such work (the project's own then).
func bookNames(id int, book metaops.LibrivoxBook, work *metaops.LibrivoxWork) (author, title string) {
	var authors []string
	workTitle := ""
	if work != nil {
		authors, workTitle = work.Authors, work.Title
	}
	if len(authors) == 0 {
		for _, a := range book.Authors {
			if name := a.Name(); name != "" {
				authors = append(authors, name)
			}
		}
	}
	switch {
	case len(authors) > variousAfter:
		author = Various
	case len(authors) > 0:
		author = authors[0]
	}
	first, second := workTitle, book.Title
	if work != nil && work.LibrivoxCount() > 1 {
		first, second = book.Title, workTitle
	}
	for _, t := range []string{first, second, "LibriVox " + strconv.Itoa(id)} {
		if title = segment(t); title != "" {
			break
		}
	}
	return segment(author), title
}

// destination is where the book's folder goes: placement's standalone rule (an
// existing author folder of the library reused as spelled, else the sanitized
// author, then the title), library-relative, with its parent folder ("" for
// the library's root: a book with no author). The title goes verbatim, never
// through match.CleanTitle (importjob.Planner's), which would drop the very
// "(version 2)" or "Vol. 001" that tells two LibriVox projects of one work apart.
func destination(author, title string, books []link.LibraryBook) (parent, dest string) {
	folder := ""
	if author != "" {
		folder = importjob.AuthorFolders(books)[match.Normalize(author)]
	}
	sug := placement.Suggest(placement.Input{Author: author, Title: title, TitleShort: title, AuthorFolder: folder})
	dest = strings.Trim(sug.RelPath, "/")
	parent = path.Dir(dest)
	if parent == "." {
		parent = ""
	}
	return parent, dest
}

// fallbackName is the folder name tried once when the title's is taken.
func fallbackName(title string, id int) string {
	return fmt.Sprintf("%s (LibriVox %d)", title, id)
}

// candidates are the folders a fetch planned for dest may place the book at:
// dest, then once its fallback name, unless dest is the fallback already.
func candidates(dest string, id int) []string {
	if strings.HasSuffix(dest, fallbackName("", id)) {
		return []string{dest}
	}
	return []string{dest, fallbackName(dest, id)}
}

// audioExts are the extensions a section file keeps from its URL; anything
// else is named .mp3 (LibriVox publishes its sections as MP3).
var audioExts = map[string]bool{".mp3": true, ".ogg": true, ".m4a": true, ".m4b": true, ".flac": true, ".opus": true}

// sectionNames are the sections' file names: "NN - <title>.<ext>", NN the
// 1-based position (never LibriVox's section_number, which starts at 0 on some
// projects), zero-padded to the count's width and at least two digits, and a
// section with no title left "Section N".
func sectionNames(sections []metaops.LibrivoxSection) []string {
	width := max(2, len(strconv.Itoa(len(sections))))
	out := make([]string, len(sections))
	for i, s := range sections {
		n := i + 1
		name := segment(s.Title)
		if name == "" {
			name = "Section " + strconv.Itoa(n)
		}
		out[i] = fmt.Sprintf("%0*d - %s%s", width, n, name, sectionExt(s.ListenURL))
	}
	return out
}

// sectionExt is the file's extension from its URL's path (lower case), .mp3
// when it names no audio one.
func sectionExt(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if ext := strings.ToLower(path.Ext(u.Path)); audioExts[ext] {
			return ext
		}
	}
	return ".mp3"
}

// AllowedURL reports whether a fetch may request u (the first URL and every
// redirect's, netguard checking the addresses beside it): https on the default
// port, no credentials, and archive.org or a host under it. Never librivox.org
// (LIBRIVOX-PLAN.md L6 decision 7): the Bridge reads LibriVox through meta and
// its files from the Internet Archive only.
func AllowedURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "archive.org" || (strings.HasSuffix(host, ".archive.org") && len(host) > len(".archive.org"))
}

// allowedRaw is AllowedURL over a URL string, as it is requested: a padded
// one is refused here rather than checked trimmed and then sent as it is.
func allowedRaw(raw string) bool {
	if raw != strings.TrimSpace(raw) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && AllowedURL(u)
}
