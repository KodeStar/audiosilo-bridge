package podcast

import "encoding/json"

const (
	Version      = 1
	MaxEpisodes  = 20000
	MaxFileBytes = 24 << 20
)

type Feed struct {
	Version               int       `json:"version"`
	Generator             string    `json:"generator"`
	ShowID                string    `json:"show_id"`
	FeedURL               string    `json:"feed_url"`
	FeedURLSHA256         string    `json:"feed_url_sha256"`
	PreviousFeedURLSHA256 []string  `json:"previous_feed_url_sha256"`
	Private               bool      `json:"private"`
	FeedHost              string    `json:"feed_host"`
	SubscribedAt          string    `json:"subscribed_at"`
	UpdatedAt             string    `json:"updated_at"`
	Show                  Show      `json:"show"`
	Episodes              []Episode `json:"episodes"`
}
type Show struct {
	Title       string   `json:"title"`
	Author      string   `json:"author"`
	Description string   `json:"description"`
	Link        string   `json:"link"`
	Language    string   `json:"language"`
	Type        string   `json:"type"`
	Explicit    bool     `json:"explicit"`
	Categories  []string `json:"categories"`
	PodcastGUID string   `json:"podcast_guid"`
	Image       *Image   `json:"image"`
}
type Image struct {
	File         string `json:"file"`
	SourceSHA256 string `json:"source_sha256"`
	FetchedAt    string `json:"fetched_at"`
}
type Episode struct {
	ID              string       `json:"id"`
	GUID            string       `json:"guid"`
	Path            string       `json:"path"`
	Title           string       `json:"title"`
	Published       string       `json:"published"`
	Description     string       `json:"description"`
	Season          *int         `json:"season"`
	Episode         *int         `json:"episode"`
	EpisodeType     string       `json:"episode_type"`
	Explicit        bool         `json:"explicit"`
	DurationSeconds *int         `json:"duration_seconds"`
	Enclosure       Enclosure    `json:"enclosure"`
	ChaptersURL     string       `json:"chapters_url"`
	Transcripts     []Transcript `json:"transcripts"`
	FirstSeen       string       `json:"first_seen"`
	RemovedAt       string       `json:"removed_at"`
	Fetch           *Fetch       `json:"fetch"`
}
type Enclosure struct {
	URL    string `json:"url"`
	Type   string `json:"type"`
	Length int64  `json:"length"`
}
type Transcript struct {
	URL      string `json:"url"`
	Type     string `json:"type"`
	Language string `json:"language"`
}
type Fetch struct {
	Generation      int    `json:"generation"`
	SHA256          string `json:"sha256"`
	Bytes           int64  `json:"bytes"`
	DurationMS      int64  `json:"duration_ms"`
	ContentType     string `json:"content_type"`
	FetchedAt       string `json:"fetched_at"`
	DeletedAt       string `json:"deleted_at"`
	TimelineChanged bool   `json:"timeline_changed"`
}

// ScrubPrivate is applied immediately before every private feed write.
func (f *Feed) ScrubPrivate() {
	if !f.Private {
		return
	}
	f.FeedURL, f.Show.Link = "", ""
	f.Show.Title = RedactText(f.Show.Title)
	f.Show.Author = RedactText(f.Show.Author)
	f.Show.Description = RedactText(f.Show.Description)
	for i := range f.Show.Categories {
		f.Show.Categories[i] = RedactText(f.Show.Categories[i])
	}
	if urlInText.MatchString(f.Show.PodcastGUID) {
		f.Show.PodcastGUID = "sha256:" + FeedURLHash(f.Show.PodcastGUID)
	}
	for i := range f.Episodes {
		e := &f.Episodes[i]
		if urlInText.MatchString(e.GUID) {
			e.GUID = "sha256:" + FeedURLHash(e.GUID)
		}
		e.Title = RedactText(e.Title)
		e.Description = RedactText(e.Description)
		e.Enclosure.Type = RedactText(e.Enclosure.Type)
		e.Enclosure.URL, e.ChaptersURL = "", ""
		e.Transcripts = []Transcript{}
	}
}

// RedactParams is also used at the page API boundary, including legacy rows.
func RedactParams(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return json.RawMessage(`{}`)
	}
	if v, ok := fields["feed_url"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			fields["feed_url"], _ = json.Marshal(RedactURL(s))
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

func RedactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return json.RawMessage(`{}`)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return RedactText(x)
		case map[string]any:
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		}
		return v
	}
	out, err := json.Marshal(walk(value))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}
