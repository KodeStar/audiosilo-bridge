package podcast

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var reservedName = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\..*)?$`)

func ShowName(title string, entries []string) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\ufffd' || r == '\u061c' || r == '\u200e' || r == '\u200f' || r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069' {
			return -1
		}
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return ' '
		}
		return r
	}, norm.NFC.String(title))
	s = strings.Trim(Clip(strings.Join(strings.Fields(s), " "), 100), " .")
	if s == "" {
		s = "Podcast"
	}
	if reservedName.MatchString(s) {
		s = Clip(s, 90) + " (podcast)"
	}
	used := map[string]bool{}
	for _, name := range entries {
		used[foldName(name)] = true
	}
	base := s
	for n := 2; used[foldName(s)]; n++ {
		suffix := fmt.Sprintf(" (%d)", n)
		s = strings.TrimRight(Clip(base, 100-len(suffix)), " .") + suffix
	}
	return s
}
func foldName(s string) string { return cases.Fold().String(norm.NFC.String(s)) }
func ValidShow(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.HasPrefix(s, ".") && !strings.ContainsAny(s, "/\\\x00") && path.Clean(s) == s
}

// EnclosureBase is an enclosure URL without its fragment, and without its query
// when the path names an audio file (a tracking value there changes while the
// file stays the same, BRIDGE-PLAN spike 3). A path naming no file
// ("/uc?export=download&id=...") keeps the query that tells its episodes apart.
func EnclosureBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	if IsAudioExtension(strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")) {
		u.RawQuery, u.ForceQuery = "", false
	}
	return u.String()
}
func Identity(e Episode) string {
	if e.GUID != "" {
		return "guid:" + e.GUID
	}
	if e.Enclosure.URL != "" {
		return "url:" + EnclosureBase(e.Enclosure.URL)
	}
	return "title:" + e.Title + "|" + e.Published
}
func EpisodeID(e Episode) string { return FeedURLHash(Identity(e))[:16] }

// EpisodePath is assigned once. used includes retained episodes and disk entries.
func EpisodePath(show string, e Episode, ext string, used map[string]bool) string {
	date := "undated"
	if t, err := time.Parse(time.RFC3339, e.Published); err == nil {
		date = t.UTC().Format("2006-01-02")
	}
	hash := FeedURLHash(Identity(e))
	base := show + "/" + date + "-"
	name := base + hash[:10] + "." + ext
	if !used[foldName(name)] {
		return name
	}
	name = base + hash[:16] + "." + ext
	for n := 2; used[foldName(name)]; n++ {
		name = fmt.Sprintf("%s%s-%d.%s", base, hash[:16], n, ext)
	}
	return name
}

func Extension(typ, rawURL string) string {
	typ = strings.ToLower(strings.TrimSpace(strings.SplitN(typ, ";", 2)[0]))
	switch typ {
	case "audio/mpeg", "audio/mp3", "audio/mpeg3", "audio/x-mpeg", "audio/x-mp3", "audio/x-mpeg3", "audio/mpg", "audio/x-mpg":
		return "mp3"
	case "audio/mp4", "audio/x-m4a", "audio/m4a":
		return "m4a"
	case "audio/ogg", "audio/vorbis":
		return "ogg"
	case "audio/opus":
		return "opus"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "", "application/octet-stream":
		u, e := url.Parse(rawURL)
		if e != nil {
			return ""
		}
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(u.Path), "."))
		if IsAudioExtension(ext) {
			return ext
		}
	}
	return ""
}
func IsAudioExtension(ext string) bool {
	switch ext {
	case "mp3", "m4a", "m4b", "mp4", "ogg", "opus", "flac":
		return true
	}
	return false
}
