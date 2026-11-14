package metaops

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// The LibriVox reads librivox.fetch makes (LIBRIVOX-PLAN.md L6, decision 5):
// the project's manifest from meta's LibriVox-shaped feed
// (GET /api/v1/librivox/audiobooks?id=N&extended=1&coverart=1, LibriVox's own
// shape) and the work it belongs to (GET /api/v1/works/{id}), uncached: a
// fetch reads each once per run.

// ErrNotFound is a clean 404 from meta: no such project or work.
var ErrNotFound = errors.New("not found in the metadata database")

// ErrUnavailable is meta unreachable or answering something unreadable (a
// transport failure, a 5xx, a body that isn't the shape).
var ErrUnavailable = errors.New("the metadata service is unavailable")

// ErrRefused is a permanent 4xx from meta other than a 404 (a 400, 403, 410):
// asking again won't change it. A 408, 425 or 429 is ErrUnavailable.
var ErrRefused = errors.New("the metadata service refused the request")

// LibrivoxBook is one project of the LibriVox-shaped feed, the fields a fetch
// reads. LibriVox's ids are strings, and coverart_jpg is null when the project
// states none.
type LibrivoxBook struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Authors     []LibrivoxPerson  `json:"authors"`
	URLIArchive string            `json:"url_iarchive"`
	CoverartJPG *string           `json:"coverart_jpg"`
	Sections    []LibrivoxSection `json:"sections"`
}

// LibrivoxPerson is an author as the feed names one (the stored name split at
// its last word).
type LibrivoxPerson struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// Name is the person's name as stored ("Franz Kafka", "Voltaire").
func (p LibrivoxPerson) Name() string {
	return strings.TrimSpace(strings.TrimSpace(p.FirstName) + " " + strings.TrimSpace(p.LastName))
}

// LibrivoxSection is one section (one file) of a project.
type LibrivoxSection struct {
	// SectionNumber is LibriVox's own number for the section (from 0 on some
	// projects), a string as LibriVox sends it.
	SectionNumber string `json:"section_number"`
	Title         string `json:"title"`
	ListenURL     string `json:"listen_url"`
}

// librivoxDetails is the prefix the feed states url_iarchive with.
const librivoxDetails = "/details/"

// ArchiveID is the Internet Archive item of the project, from url_iarchive
// ("https://www.archive.org/details/<item>"), "" when it states none.
func (b LibrivoxBook) ArchiveID() string {
	u, err := url.Parse(strings.TrimSpace(b.URLIArchive))
	if err != nil {
		return ""
	}
	_, item, ok := strings.Cut(u.Path, librivoxDetails)
	if !ok {
		return ""
	}
	item, _, _ = strings.Cut(item, "/")
	return item
}

// LibrivoxAudiobook reads LibriVox project id from meta's feed, with its
// sections and cover. ErrDisabled: no base URL; ErrNotFound: meta has no such
// project; ErrRefused: meta refuses the request for good; ErrUnavailable: meta
// can't be read now.
func (c *Client) LibrivoxAudiobook(ctx context.Context, id int) (LibrivoxBook, error) {
	if !c.Enabled() {
		return LibrivoxBook{}, ErrDisabled
	}
	q := url.Values{"id": {strconv.Itoa(id)}, "extended": {"1"}, "coverart": {"1"}}
	var res struct {
		Books []LibrivoxBook `json:"books"`
	}
	if err := readErr(ctx, c.readJSON(ctx, "/api/v1/librivox/audiobooks?"+q.Encode(), &res)); err != nil {
		return LibrivoxBook{}, err
	}
	for _, b := range res.Books {
		if b.ID == strconv.Itoa(id) {
			return b, nil
		}
	}
	return LibrivoxBook{}, ErrNotFound
}

// LibrivoxWork is a work as a fetch reads it: its title, its authors' names
// and its recordings' LibriVox identities.
type LibrivoxWork struct {
	Title      string
	Authors    []string
	Recordings []LibrivoxRecording
}

// LibrivoxRecording is one recording of a work: its id, the LibriVox project
// it is (0: not a LibriVox recording, or a metaserve too old to say) and its
// Internet Archive item.
type LibrivoxRecording struct {
	ID         string
	LibrivoxID int
	ArchiveID  string
}

// LibrivoxCount is how many of the work's recordings are LibriVox projects.
func (w LibrivoxWork) LibrivoxCount() int {
	n := 0
	for _, r := range w.Recordings {
		if r.LibrivoxID > 0 {
			n++
		}
	}
	return n
}

// readErr is a readJSON outcome as these reads' error: the caller's cancel
// before anything else, else the outcome.
func readErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// LibrivoxWork reads a work for a LibriVox fetch. ErrDisabled, ErrNotFound,
// ErrRefused and ErrUnavailable as LibrivoxAudiobook's.
func (c *Client) LibrivoxWork(ctx context.Context, workID string) (LibrivoxWork, error) {
	if !c.Enabled() {
		return LibrivoxWork{}, ErrDisabled
	}
	res, err := c.fetchWork(ctx, workID)
	if err := readErr(ctx, err); err != nil {
		return LibrivoxWork{}, err
	}
	w := LibrivoxWork{Title: res.Title}
	for _, a := range res.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			w.Authors = append(w.Authors, name)
		}
	}
	for _, r := range res.Recordings {
		w.Recordings = append(w.Recordings, LibrivoxRecording{ID: r.ID, LibrivoxID: r.LibrivoxID, ArchiveID: r.ArchiveID})
	}
	return w, nil
}
