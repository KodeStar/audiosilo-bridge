package podcast

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSharedURLVectorsAndPrivacy(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("testdata/url-vectors.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("bad vector: %s", line)
		}
		got, e := NormalizeFeedURL(parts[0])
		if e != nil || got != parts[1] || FeedURLHash(got) != parts[2] {
			t.Errorf("normalize %q: %q %v hash %s", parts[0], got, e, FeedURLHash(got))
		}
	}
	for _, s := range []string{"ftp://x/y", "/relative", "https:///x", "https://" + strings.Repeat("x", 2041), "https://x:invalid/", "https://x/%zz"} {
		if _, e := NormalizeFeedURL(s); !errors.Is(e, ErrInvalidFeedURL) {
			t.Errorf("accepted %q", s)
		}
	}
	for s, want := range map[string]bool{"https://example.com/feed": false, "http://example.com/": false, "https://u:p@example.com": true, "https://example.com/?": true, "https://example.com/?a=1": true, "https://sub.patreon.com/feed": true, "https://otherpatreon.com/feed": false, "https://show.substack.com/private/rss": true, "https://example.com/Abcdefghijklmnopqrstu1234/rss": true, "https://example.com/abcdefghijklmnopqrstuvwx/rss": false} {
		n, e := NormalizeFeedURL(s)
		if e != nil || IsPrivateFeedURL(n) != want {
			t.Errorf("privacy %s: %v %v", s, IsPrivateFeedURL(n), e)
		}
	}
	if got := RedactURL("https://user:pass@example.com:8443/feed?secret=123"); got != "https://example.com:8443/..." {
		t.Fatal(got)
	}
	if RedactURL("http://example.com/") != "http://example.com" {
		t.Fatal("root redaction")
	}
	if n, e := NormalizeFeedURL("HTTPS://u:%70%61ss@EXAMPLE.COM?x=@"); e != nil || n != "https://u:%70%61ss@example.com/?x=@" {
		t.Fatalf("verbatim userinfo: %s %v", n, e)
	}
}

func fixture(t *testing.T) *Feed {
	t.Helper()
	b, err := os.ReadFile("testdata/feed-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f Feed
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return &f
}
func TestGoldenRoundTripAndNaming(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	used := map[string]bool{}
	for _, e := range f.Episodes {
		if EpisodeID(e) != e.ID {
			t.Errorf("identity: %s", Identity(e))
		}
		if p := EpisodePath("Example Show", e, Extension(e.Enclosure.Type, e.Enclosure.URL), used); p != e.Path {
			t.Errorf("path %s want %s", p, e.Path)
		}
		used[foldName(e.Path)] = true
	}
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, "Example Show"), 0755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	changed, err := Write(root, "Example Show", f, time.Now())
	if err != nil || !changed {
		t.Fatalf("write: %v %v", changed, err)
	}
	got, err := Read(root, "Example Show")
	if err != nil || !reflect.DeepEqual(f, got) {
		t.Fatalf("round trip: %v", err)
	}
	info, _ := root.Stat("Example Show/feed.json")
	changed, err = Write(root, "Example Show", f, time.Now().Add(time.Hour))
	after, _ := root.Stat("Example Show/feed.json")
	if err != nil || changed || !info.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged write changed mtime")
	}
	for input, want := range map[string]string{"../CON": "CON (podcast)", "..A:/ B\\ C?*\"<>|..": "A B C", "\u202e\x01\ufffd": "Podcast", "NUL.txt": "NUL.txt (podcast)", "cafe\u0301": "café"} {
		if got := ShowName(input, nil); got != want {
			t.Errorf("show %q: %q want %q", input, got, want)
		}
	}
	if got := ShowName("cafe\u0301", []string{"CAFÉ", "café (2)"}); got != "café (3)" {
		t.Fatal(got)
	}
	name := ShowName(strings.Repeat("界", 100), []string{strings.Repeat("界", 33)})
	if len(name) > 100 || !utf8.ValidString(name) {
		t.Fatal("unsafe name truncation")
	}
	e := f.Episodes[0]
	first := EpisodePath("Example Show", e, "mp3", nil)
	used = map[string]bool{foldName(first): true}
	second := EpisodePath("Example Show", e, "mp3", used)
	used[foldName(second)] = true
	third := EpisodePath("Example Show", e, "mp3", used)
	if second == first || !strings.HasSuffix(third, "-2.mp3") {
		t.Fatal(first, second, third)
	}
}

const rss = `<rss version="2.0" xmlns:i="http://www.itunes.com/DTDs/PodCast-1.0.dtd" xmlns:pc="https://podcastindex.org/namespace/1.0"><channel><title>Show &amp; more</title><description><![CDATA[<b>Notes</b><script>secret</script> now]]></description><pc:guid>show-guid</pc:guid><i:type>serial</i:type><i:author>Author</i:author><i:new-feed-url>https://new.example/feed</i:new-feed-url><item><guid>ep1</guid><title>Episode</title><pubDate>Mon, 19 Oct 2026 21:00:00 +0000</pubDate><i:duration>01:02:03</i:duration><i:season>2</i:season><i:episode>3</i:episode><i:episodeType>trailer</i:episodeType><i:explicit>yes</i:explicit><enclosure url="/audio.mp3?token=SECRET123" type="audio/mpeg" length="123"/><pc:chapters url="/chapters.json" type="application/json+chapters"/><pc:transcript url="/a.vtt" type="text/vtt" language="en"/><pc:transcript url="/a.srt" type="application/srt" language="fr"/></item><item><guid>video</guid><enclosure url="/movie.mp4" type="video/mp4"/></item><item><enclosure url="/unsupported.aac" type="audio/aac"/></item></channel></rss>`

func TestParseNamespacesTextAndEnclosures(t *testing.T) {
	t.Parallel()
	p, err := Parse([]byte(rss), "https://example.com/feed")
	if err != nil {
		t.Fatal(err)
	}
	if p.Show.PodcastGUID != "show-guid" || p.Show.Type != "serial" || p.Show.Author != "Author" || p.Show.Description != "Notes now" || len(p.Episodes) != 1 || p.Skipped != 2 {
		t.Fatalf("parse %+v", p)
	}
	e := p.Episodes[0]
	if e.ChaptersURL != "https://example.com/chapters.json" || len(e.Transcripts) != 2 || e.Transcripts[1].Language != "fr" || e.Season == nil || *e.Season != 2 || *e.DurationSeconds != 3723 || !e.Explicit || e.EpisodeType != "trailer" {
		t.Fatalf("episode %+v", e)
	}
	fake := strings.ReplaceAll(rss, podcastNamespace, "https://other.example/ns")
	relativeMove := strings.ReplaceAll(rss, "https://new.example/feed", "/new-feed")
	if parsed, e := Parse([]byte(relativeMove), "https://example.com/feed"); e != nil || parsed.NewFeedURL != "" {
		t.Fatal("relative new-feed-url accepted", e)
	}
	p, err = Parse([]byte(fake), "https://example.com/feed")
	if err != nil || p.Show.PodcastGUID != "" || p.Episodes[0].ChaptersURL != "" {
		t.Fatal("accepted wrong namespace")
	}
	if _, err := Parse([]byte("<html>login</html>"), "https://example.com/feed"); !errors.Is(err, ErrNotFeed) {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(`<rss version="2.0"><channel><title>News</title><item><title>A blog</title></item></channel></rss>`), "https://example.com/feed"); !errors.Is(err, ErrNotPodcast) {
		t.Fatal(err)
	}
	for typ, want := range map[string]string{"audio/mpeg": "mp3", "audio/mp3": "mp3", "audio/mpeg3": "mp3", "audio/x-mpeg": "mp3", "audio/x-mp3": "mp3", "audio/x-mpeg3": "mp3", "audio/mpg": "mp3", "audio/x-mpg": "mp3", "audio/mp4": "m4a", "audio/m4a": "m4a", "audio/x-m4a": "m4a", "audio/ogg": "ogg", "audio/vorbis": "ogg", "audio/opus": "opus", "audio/x-flac": "flac", "audio/flac": "flac", "": "m4b", "application/octet-stream": "m4b", "audio/aac": "", "audio/wav": "", "video/mp4": ""} {
		if got := Extension(typ, "https://example.com/a.m4b?token=x"); got != want {
			t.Errorf("extension %s = %s", typ, got)
		}
	}
}
func TestMergeStablePathsRemovedAndAliases(t *testing.T) {
	t.Parallel()
	f := fixture(t)
	kept := f.Episodes[0]
	p := &Parsed{Show: f.Show, Episodes: []Episode{kept, kept, f.Episodes[1]}}
	p.Episodes[0].GUID = "changed-guid"
	p.Episodes[0].Published = "2026-10-18T00:00:00Z"
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	added, err := Merge(f, p, "Example Show", now)
	if err != nil || added != 0 || len(f.Episodes) != 3 || f.Episodes[0].Path != kept.Path || f.Episodes[0].ID != kept.ID || f.Episodes[2].RemovedAt == "" || f.Episodes[1].Fetch == nil {
		t.Fatalf("merge %+v %d %v", f.Episodes, added, err)
	}
	returned := f.Episodes[2]
	returned.RemovedAt = ""
	p.Episodes = append(p.Episodes, returned)
	_, err = Merge(f, p, "Example Show", now)
	if err != nil || f.Episodes[2].RemovedAt != "" {
		t.Fatal("return did not clear removed_at")
	}
	f.Private = true
	p.Episodes = []Episode{{GUID: "https://example.com/SECRET123?token=SECRET123", Title: "Private", Published: "", Enclosure: Enclosure{URL: "https://example.com/a.mp3?token=SECRET123", Type: "audio/mpeg"}, ChaptersURL: "https://example.com/SECRET123", Transcripts: []Transcript{{URL: "https://example.com/SECRET123"}}}}
	added, err = Merge(f, p, "Example Show", now)
	if err != nil || added != 1 {
		t.Fatal(added, err)
	}
	raw, _ := json.Marshal(f)
	if strings.Contains(string(raw), "SECRET123") {
		t.Fatal("private URL leaked into feed.json")
	}
	added, err = Merge(f, p, "Example Show", now)
	if err != nil || added != 0 {
		t.Fatal("private identity not stable", added, err)
	}
}
func TestMergeOwnIdentityFirstAndQueryIdentifiedEnclosures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	ep := func(guid, title, url string) Episode {
		return Episode{GUID: guid, Title: title, EpisodeType: "full", Enclosure: Enclosure{URL: url, Type: "audio/mpeg"}, Transcripts: []Transcript{}}
	}
	// A new item that only shares a kept episode's title and date, listed before
	// that episode's own item, is a new episode: it never takes the kept one.
	f := &Feed{Episodes: []Episode{}}
	old := ep("old", "Bonus", "https://cdn.example.com/old.mp3")
	if _, err := Merge(f, &Parsed{Episodes: []Episode{old}}, "Show", now); err != nil {
		t.Fatal(err)
	}
	kept := f.Episodes[0]
	added, err := Merge(f, &Parsed{Episodes: []Episode{ep("new", "Bonus", "https://cdn.example.com/new.mp3"), old}}, "Show", now)
	if err != nil || added != 1 || len(f.Episodes) != 2 {
		t.Fatalf("merge: %d %v %+v", added, err, f.Episodes)
	}
	for _, e := range f.Episodes {
		if e.GUID == "old" && (e.ID != kept.ID || e.Path != kept.Path || e.RemovedAt != "") {
			t.Fatalf("the kept episode lost its identity: %+v", e)
		}
		if e.GUID == "new" && (e.ID == kept.ID || e.Path == kept.Path) {
			t.Fatalf("a weak match took the kept episode: %+v", e)
		}
	}
	// Enclosures told apart only by their query (a download link) stay apart; a
	// tracking value on an audio file's path is still the same file.
	f = &Feed{Episodes: []Episode{}}
	drive := func(id string) Episode {
		return ep("", "Episode "+id, "https://drive.example.com/uc?export=download&id="+id)
	}
	added, err = Merge(f, &Parsed{Episodes: []Episode{drive("A"), drive("B")}}, "Show", now)
	if err != nil || added != 2 || len(f.Episodes) != 2 || f.Episodes[0].Path == f.Episodes[1].Path {
		t.Fatalf("query-identified enclosures collapsed: %d %v %+v", added, err, f.Episodes)
	}
	if got := EnclosureBase("https://cdn.example.com/a.MP3?ts=1#t=5"); got != "https://cdn.example.com/a.MP3" {
		t.Fatal(got)
	}
	if got := EnclosureBase("https://drive.example.com/uc?export=download&id=A#x"); got != "https://drive.example.com/uc?export=download&id=A" {
		t.Fatal(got)
	}
}
func TestMergeCapDropsTheLongestRemoved(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	f := &Feed{Episodes: make([]Episode, 0, MaxEpisodes)}
	for i := range MaxEpisodes {
		f.Episodes = append(f.Episodes, Episode{ID: fmt.Sprintf("id%05d", i), GUID: fmt.Sprintf("g%d", i), Path: fmt.Sprintf("Show/%05d.mp3", i), Title: fmt.Sprintf("Old %d", i), EpisodeType: "full", Transcripts: []Transcript{},
			RemovedAt: now.Add(-time.Duration(MaxEpisodes-i) * time.Hour).Format(time.RFC3339)})
	}
	f.Episodes[0].Fetch = &Fetch{Generation: 1} // fetched: never dropped
	fresh := Episode{GUID: "fresh", Title: "Fresh", EpisodeType: "full", Enclosure: Enclosure{URL: "https://cdn.example.com/fresh.mp3", Type: "audio/mpeg"}, Transcripts: []Transcript{}}
	// A feed rotating its items through the cap keeps updating: the removed
	// episode gone longest (and never fetched) makes room.
	added, err := Merge(f, &Parsed{Episodes: []Episode{fresh}}, "Show", now)
	if err != nil || added != 1 || len(f.Episodes) != MaxEpisodes {
		t.Fatalf("merge at the cap: %d %v %d", added, err, len(f.Episodes))
	}
	has := func(guid string) bool {
		return slices.ContainsFunc(f.Episodes, func(e Episode) bool { return e.GUID == guid })
	}
	if !has("fresh") || !has("g0") || has("g1") || !has("g2") {
		t.Fatal("dropped the wrong episode")
	}
	// Episodes the feed still lists are never dropped.
	items := make([]Episode, 0, MaxEpisodes+1)
	for i := range MaxEpisodes + 1 {
		items = append(items, Episode{GUID: fmt.Sprintf("live%d", i), Title: "Live", EpisodeType: "full", Enclosure: Enclosure{URL: fmt.Sprintf("https://cdn.example.com/%d.mp3", i), Type: "audio/mpeg"}, Transcripts: []Transcript{}})
	}
	if _, err := Merge(&Feed{Episodes: []Episode{}}, &Parsed{Episodes: items}, "Show", now); !errors.Is(err, ErrTooLarge) {
		t.Fatal("a feed over the cap was accepted", err)
	}
}
func TestTextKeepsWhatOnlyLooksLikeMarkup(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Why a<b matters":                     "Why a<b matters",
		"Rust: Vec<T> explained":              "Rust: Vec<T> explained",
		"Generic <K, V> maps":                 "Generic <K, V> maps",
		"x<y &amp; z":                         "x<y & z",
		"<b>Bold</b> &amp; co":                "Bold & co",
		"<p>Notes</p><script>secret</script>": "Notes",
		"a<o:p></o:p>b":                       "a b",
		"AT&amp;T":                            "AT&T",
		"a < b":                               "a < b",
	} {
		if got := Text(in, 300); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}
func TestLegacyCharsetFeedKeepsPodcastNamespace(t *testing.T) {
	t.Parallel()
	feed := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<rss version=\"2.0\" xmlns:podcast=\"https://podcastindex.org/namespace/1.0\"><channel><title>Caf\xe9</title><podcast:guid>show-guid</podcast:guid><item><guid>g1</guid><title>One</title><enclosure url=\"https://example.com/1.mp3\" type=\"audio/mpeg\"/><podcast:transcript url=\"https://example.com/1.vtt\" type=\"text/vtt\"/></item></channel></rss>"
	p, err := Parse([]byte(feed), "https://example.com/feed")
	if err != nil {
		t.Fatal(err)
	}
	if p.Show.Title != "Caf\u00e9" || p.Show.PodcastGUID != "show-guid" || len(p.Episodes) != 1 || len(p.Episodes[0].Transcripts) != 1 {
		t.Fatalf("legacy charset feed lost podcast data: %+v", p)
	}
}
func TestDurationTakesFractionalSeconds(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int{"1503": 1503, "1503.4": 1503, "25:03.400": 1503, "00:25:03.9": 1503, "01:02:03": 3723} {
		if got := duration(in); got == nil || *got != want {
			t.Errorf("duration(%q) = %v, want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "1503.", ".4", "1503.4.5", "25:03.x", "1:2:3:4", "-5"} {
		if got := duration(in); got != nil {
			t.Errorf("duration(%q) = %d, want none", in, *got)
		}
	}
}
func TestRedactTextKeepsClosingPunctuation(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"see https://u:p@host.example/feed?token=SECRET123.":     "see https://host.example/....",
		"(https://host.example/a?token=SECRET123), then":         "(https://host.example/...), then",
		"dial https://host.example: refused":                     "dial https://host.example: refused",
		"fetch https://host.example/feed.xml?k=SECRET123; retry": "fetch https://host.example/...; retry",
	} {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%q) = %q, want %q", in, got, want)
		}
	}
}
func TestIntervalAndReadRefusals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for days, want := range map[int]int{0: 1800, 3: 7200, 15: 21600, 61: 86400} {
		f := &Feed{Episodes: []Episode{{Published: now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)}}}
		if got := NextPoll(f, "", now); got != want {
			t.Errorf("%d days: %d", days, got)
		}
		if got := NextPoll(f, "public, max-age=999999", now); got != 86400 {
			t.Fatal(got)
		}
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Show"), 0755)
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	os.Symlink(filepath.Join(dir, "outside"), filepath.Join(dir, "Show", "feed.json"))
	if _, err := Read(root, "Show"); err == nil {
		t.Fatal("symlinked feed accepted")
	}
	if WriteAtomic(root, "Show", "feed.json", []byte("x")) == nil {
		t.Fatal("write accepted symlink")
	}
	os.Remove(filepath.Join(dir, "Show", "feed.json"))
	f := fixture(t)
	f.Version = 2
	b, _ := json.Marshal(f)
	os.WriteFile(filepath.Join(dir, "Show", "feed.json"), b, 0644)
	if _, err := Read(root, "Show"); err == nil {
		t.Fatal("future version accepted")
	}
	if WriteAtomic(root, "../escape", "feed.json", nil) == nil {
		t.Fatal("write escaped root")
	}
}

func TestFileCapDropsOnlyRemovedDescriptions(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Example Show"), 0755)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f := fixture(t)
	f.Episodes[2].Description = strings.Repeat("x", MaxFileBytes)
	changed, err := Write(root, "Example Show", f, time.Now())
	if err != nil || !changed || f.Episodes[2].Description != "" {
		t.Fatalf("removed description cap: %v", err)
	}
	f.Episodes[0].Description = strings.Repeat("x", MaxFileBytes)
	if _, err := Write(root, "Example Show", f, time.Now()); !errors.Is(err, ErrTooLarge) {
		t.Fatal("oversized active feed accepted", err)
	}
	info, _ := root.Stat("Example Show/feed.json")
	if info.Size() > MaxFileBytes || info.Mode().Perm() != 0644 {
		t.Fatal("bad file cap or mode", info)
	}
}
