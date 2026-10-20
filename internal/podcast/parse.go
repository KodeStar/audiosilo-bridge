package podcast

import (
	"bytes"
	"encoding/xml"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	ext "github.com/mmcdole/gofeed/extensions"
	"golang.org/x/net/html"
)

var ErrNotFeed = errors.New("not_a_feed")
var ErrNotPodcast = errors.New("not_a_podcast")
var ErrTooLarge = errors.New("too_large")

const podcastNamespace = "https://podcastindex.org/namespace/1.0"

type Parsed struct {
	Show       Show
	Episodes   []Episode
	Skipped    int
	ImageURL   string
	NewFeedURL string
}

// Parse never fetches URLs. Spike 5 confirmed that gofeed retains every tested
// namespace attribute, including multiple transcripts, in Extensions.
func Parse(raw []byte, base string) (*Parsed, error) {
	return parse(raw, base, false)
}

// ParseExisting allows an established podcast to temporarily contain no items.
func ParseExisting(raw []byte, base string) (*Parsed, error) { return parse(raw, base, true) }

func parse(raw []byte, base string, allowEmpty bool) (*Parsed, error) {
	if len(raw) > 64<<20 {
		return nil, ErrTooLarge
	}
	f, err := gofeed.NewParser().Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrNotFeed
	}
	prefixes := podcastPrefixes(raw)
	baseURL, _ := url.Parse(base) // nil when invalid: absoluteURL then answers ""
	out := &Parsed{Show: Show{Title: Text(f.Title, 300), Description: Text(f.Description, 4000), Link: absoluteURL(baseURL, f.Link), Language: Clip(f.Language, 35), Type: "episodic", Categories: []string{}}, Episodes: []Episode{}}
	for _, p := range f.Authors {
		if p != nil && p.Name != "" {
			out.Show.Author = Text(p.Name, 300)
			break
		}
	}
	for _, c := range f.Categories {
		if len(out.Show.Categories) == 10 {
			break
		}
		out.Show.Categories = append(out.Show.Categories, Text(c, 300))
	}
	if f.ITunesExt != nil {
		i := f.ITunesExt
		if i.Author != "" {
			out.Show.Author = Text(i.Author, 300)
		}
		out.Show.Explicit = explicit(i.Explicit)
		if i.Type == "serial" {
			out.Show.Type = "serial"
		}
		if normalized, err := NormalizeFeedURL(i.NewFeedURL); err == nil {
			out.NewFeedURL = normalized
		}
		out.ImageURL = absoluteURL(baseURL, i.Image)
	}
	if out.ImageURL == "" && f.Image != nil {
		out.ImageURL = absoluteURL(baseURL, f.Image.URL)
	}
	if xs := podcastExtensions(f.Extensions, prefixes, "guid"); len(xs) > 0 {
		out.Show.PodcastGUID = Clip(strings.TrimSpace(xs[0].Value), 1024)
	}
	hasEnclosures := false
	for _, item := range f.Items {
		if item == nil {
			continue
		}
		if len(item.Enclosures) > 0 {
			hasEnclosures = true
		}
		e := Episode{GUID: Clip(strings.TrimSpace(item.GUID), 1024), Title: Text(item.Title, 500), Description: Text(item.Description, 2000), EpisodeType: "full", Transcripts: []Transcript{}}
		if e.Title == "" {
			e.Title = "Untitled episode"
		}
		if item.PublishedParsed != nil {
			e.Published = item.PublishedParsed.UTC().Format(time.RFC3339)
		}
		for _, enc := range item.Enclosures {
			if enc == nil {
				continue
			}
			u := absoluteURL(baseURL, enc.URL)
			if Extension(enc.Type, u) == "" || u == "" {
				continue
			}
			n, _ := strconv.ParseInt(enc.Length, 10, 64)
			e.Enclosure = Enclosure{URL: u, Type: Clip(enc.Type, 200), Length: max(n, 0)}
			break
		}
		if e.Enclosure.URL == "" {
			out.Skipped++
			continue
		}
		if item.ITunesExt != nil {
			i := item.ITunesExt
			e.Explicit = explicit(i.Explicit)
			e.Season = positive(i.Season)
			e.Episode = positive(i.Episode)
			e.DurationSeconds = duration(i.Duration)
			if i.EpisodeType == "trailer" || i.EpisodeType == "bonus" {
				e.EpisodeType = i.EpisodeType
			}
		}
		for _, x := range podcastExtensions(item.Extensions, prefixes, "chapters") {
			if u := absoluteURL(baseURL, x.Attrs["url"]); u != "" {
				e.ChaptersURL = u
				break
			}
		}
		for _, x := range podcastExtensions(item.Extensions, prefixes, "transcript") {
			if len(e.Transcripts) == 8 {
				break
			}
			u := absoluteURL(baseURL, x.Attrs["url"])
			if u == "" {
				continue
			}
			e.Transcripts = append(e.Transcripts, Transcript{URL: u, Type: Clip(x.Attrs["type"], 100), Language: Clip(x.Attrs["language"], 35)})
		}
		out.Episodes = append(out.Episodes, e)
		if len(out.Episodes) > MaxEpisodes {
			return nil, ErrTooLarge
		}
	}
	// An established feed may temporarily contain zero items. A feed with only
	// unsupported enclosures is a podcast with skipped entries, not an HTML page.
	hasITunes := f.ITunesExt != nil && (f.ITunesExt.Author != "" || f.ITunesExt.Type != "" || f.ITunesExt.Image != "" || f.ITunesExt.Summary != "" || len(f.ITunesExt.Categories) > 0)
	if len(out.Episodes) == 0 && !hasEnclosures && !hasITunes && len(prefixes) == 0 && (!allowEmpty || len(f.Items) != 0) {
		return nil, ErrNotPodcast
	}
	return out, nil
}

// Resolve extension prefixes by URI, never by an untrusted prefix spelling.
// Only declarations are collected; extension data comes from gofeed. The
// prefixes come back sorted and distinct.
func podcastPrefixes(raw []byte) []string {
	var out []string
	d := xml.NewDecoder(bytes.NewReader(raw))
	d.Strict = false
	for {
		tok, e := d.Token()
		if e != nil {
			break
		}
		if start, ok := tok.(xml.StartElement); ok {
			for _, a := range start.Attr {
				if a.Name.Space == "xmlns" && strings.TrimRight(strings.TrimSpace(a.Value), "/") == podcastNamespace {
					out = append(out, a.Name.Local)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
func podcastExtensions(e ext.Extensions, prefixes []string, name string) []ext.Extension {
	var out []ext.Extension
	for _, p := range prefixes {
		out = append(out, e[p][name]...)
	}
	return out
}
func absoluteURL(b *url.URL, raw string) string {
	if b == nil || strings.TrimSpace(raw) == "" {
		return ""
	}
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil {
		return ""
	}
	norm, e := NormalizeFeedURL(b.ResolveReference(u).String())
	if e != nil {
		return ""
	}
	return norm
}
func explicit(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "yes" || s == "true" || s == "explicit"
}
func positive(s string) *int {
	n, e := strconv.Atoi(strings.TrimSpace(s))
	if e != nil || n <= 0 {
		return nil
	}
	return &n
}
func duration(s string) *int {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) > 3 || s == "" {
		return nil
	}
	n := 0
	for _, p := range parts {
		v, e := strconv.Atoi(p)
		if e != nil || v < 0 || v > 31536000 {
			return nil
		}
		n = n*60 + v
	}
	if n > 31536000 {
		return nil
	}
	return &n
}

// Text strips markup, script/style content and control characters, then clips
// on a rune boundary. The byte limit also bounds the text retained in memory.
func Text(s string, limit int) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	hidden := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken:
			name, _ := z.TagName()
			if string(name) == "script" || string(name) == "style" {
				hidden++
			}
			if hidden == 0 {
				b.WriteByte(' ')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if (string(name) == "script" || string(name) == "style") && hidden > 0 {
				hidden--
			}
			if hidden == 0 {
				b.WriteByte(' ')
			}
		case html.SelfClosingTagToken:
			if hidden == 0 {
				b.WriteByte(' ')
			}
		case html.TextToken:
			if hidden == 0 {
				piece := z.Text()
				room := limit*4 - b.Len()
				b.Write(piece[:min(len(piece), max(room, 0))])
			}
		}
		if b.Len() > limit*4 {
			break
		}
	}
	text := strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			return -1
		}
		return r
	}, b.String())
	return Clip(strings.Join(strings.Fields(text), " "), limit)
}
