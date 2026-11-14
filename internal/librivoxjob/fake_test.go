package librivoxjob

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
)

// fakeMeta answers the manifest and the work.
type fakeMeta struct {
	book    metaops.LibrivoxBook
	bookErr error
	work    metaops.LibrivoxWork
	workErr error
	reads   int
	// bookErrs and workErrs: each read pops one first (nil: answer as usual).
	bookErrs, workErrs []error
}

func pop(q *[]error) error {
	if len(*q) == 0 {
		return nil
	}
	err := (*q)[0]
	*q = (*q)[1:]
	return err
}

func (m *fakeMeta) LibrivoxAudiobook(_ context.Context, id int) (metaops.LibrivoxBook, error) {
	m.reads++
	if err := pop(&m.bookErrs); err != nil {
		return metaops.LibrivoxBook{}, err
	}
	if m.bookErr != nil {
		return metaops.LibrivoxBook{}, m.bookErr
	}
	if m.book.ID != strconv.Itoa(id) {
		return metaops.LibrivoxBook{}, metaops.ErrNotFound
	}
	return m.book, nil
}

func (m *fakeMeta) LibrivoxWork(context.Context, string) (metaops.LibrivoxWork, error) {
	if err := pop(&m.workErrs); err != nil {
		return metaops.LibrivoxWork{}, err
	}
	return m.work, m.workErr
}

// remote is one file the fake archive.org serves.
type remote struct {
	body []byte
	// ctype is the Content-Type sent ("" is audio/mpeg).
	ctype  string
	etag   string
	status int // an error status instead of the file
	// redirect: the file is served from this URL, which Allow is asked about
	// first (netguard's CheckRedirect).
	redirect string
	// cut: each GET pops one; >= 0 delivers that many bytes of the answer and
	// then fails the read (a dropped connection).
	cut []int
	// onCut runs when a cut fires (a test stops the job there).
	onCut func()
}

// request is one GET the fake saw.
type request struct {
	url    string
	offset int64
	etag   string
}

// fakeFetcher is archive.org: Stream with strong-ETag ranges, Fetch for the
// cover, each URL and redirect through the request's Allow.
type fakeFetcher struct {
	mu       sync.Mutex
	files    map[string]*remote
	requests []request
}

func (f *fakeFetcher) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

func (f *fakeFetcher) resolve(raw string, allow func(*url.URL) bool) (*remote, error) {
	refused := func(at string) bool {
		u, err := url.Parse(at)
		return err != nil || (allow != nil && !allow(u))
	}
	if refused(raw) {
		return nil, &netguard.Error{Code: "blocked_host", Message: "blocked"}
	}
	r := f.files[raw]
	if r == nil {
		return &remote{status: http.StatusNotFound}, nil
	}
	if r.redirect != "" && refused(r.redirect) {
		return nil, &netguard.Error{Code: "blocked_host", Message: "blocked"}
	}
	return r, nil
}

type cutReader struct {
	r     io.Reader
	left  int
	onCut func()
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		if c.onCut != nil {
			c.onCut()
		}
		return 0, errors.New("connection reset")
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= n
	return n, err
}

func (f *fakeFetcher) Stream(_ context.Context, in netguard.StreamRequest) (*netguard.StreamResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request{url: in.URL, offset: in.Offset, etag: in.ETag})
	r, err := f.resolve(in.URL, in.Allow)
	if err != nil {
		return nil, err
	}
	if r.status != 0 {
		return &netguard.StreamResponse{Status: r.status, Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: 0}, nil
	}
	resp := &netguard.StreamResponse{Status: http.StatusOK, ETag: r.etag, ContentLength: int64(len(r.body)), ContentType: cmp.Or(r.ctype, "audio/mpeg")}
	body := r.body
	// Range only with a strong ETag, as netguard sends it.
	if in.Offset > 0 && netguard.StrongETag(in.ETag) && in.ETag == r.etag && in.Offset < int64(len(r.body)) {
		resp.Status = http.StatusPartialContent
		body = r.body[in.Offset:]
		resp.ContentLength = int64(len(body))
		resp.ContentRange = fmt.Sprintf("bytes %d-%d/%d", in.Offset, len(r.body)-1, len(r.body))
	}
	var rd io.Reader = bytes.NewReader(body)
	if len(r.cut) > 0 {
		n := r.cut[0]
		r.cut = r.cut[1:]
		if n >= 0 {
			rd = &cutReader{r: rd, left: n, onCut: r.onCut}
		}
	}
	resp.Body = io.NopCloser(rd)
	return resp, nil
}

func (f *fakeFetcher) Fetch(_ context.Context, in netguard.Request) (*netguard.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request{url: in.URL})
	r, err := f.resolve(in.URL, in.Allow)
	if err != nil {
		return nil, err
	}
	if r.status != 0 {
		return nil, &netguard.Error{Code: "http_" + strconv.Itoa(r.status), Message: "status"}
	}
	return &netguard.Response{Status: http.StatusOK, Body: r.body}, nil
}

// mp3 is an n-byte section body: an ID3 header, then fill.
func mp3(fill byte, n int) []byte {
	return append([]byte("ID3"), bytes.Repeat([]byte{fill}, n-3)...)
}

// fakeServer is the server link: the library's books and the rescan.
type fakeServer struct {
	mu        sync.Mutex
	books     []link.LibraryBook
	rescanErr []error
	rescanned []string
	answer    string // the rescan's book path ("" = the path asked)
}

func (s *fakeServer) LibraryBooks(context.Context, int64, string, int) (*link.LibraryBooks, error) {
	return &link.LibraryBooks{Books: s.books}, nil
}

func (s *fakeServer) Rescan(_ context.Context, _ int64, p string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rescanned = append(s.rescanned, p)
	if len(s.rescanErr) > 0 {
		err := s.rescanErr[0]
		s.rescanErr = s.rescanErr[1:]
		if err != nil {
			return "", err
		}
	}
	if s.answer != "" {
		return s.answer, nil
	}
	return p, nil
}

// fakeLibs is the mapping registry: library 3, books, mapped at mount.
type fakeLibs struct {
	mount    string
	writable bool
	kind     string
	// second, when set, is library 4's mount: a second books library.
	second string
}

// mountOf is library id's mount, "" for none.
func (l *fakeLibs) mountOf(id int64) string {
	switch {
	case id == 3:
		return l.mount
	case id == 4 && l.second != "":
		return l.second
	}
	return ""
}

func (l *fakeLibs) Library(id int64) (link.Library, bool) {
	if l.mountOf(id) == "" {
		return link.Library{}, false
	}
	return link.Library{ID: id, Name: "Books", Root: "/srv/books" + strconv.FormatInt(id, 10), Kind: l.kind}, true
}

func (l *fakeLibs) Status(id int64) (link.LibraryStatus, bool) {
	mount := l.mountOf(id)
	if mount == "" {
		return link.LibraryStatus{}, false
	}
	return link.LibraryStatus{LibraryID: id, MountPath: mount, Mapped: true, Writable: l.writable}, true
}

// harness is a handler over the fakes, a library folder and a state folder.
type harness struct {
	h        *Handler
	meta     *fakeMeta
	fetch    *fakeFetcher
	srv      *fakeServer
	libs     *fakeLibs
	mount    string
	stateDir string
	free     int64
	serverID string
	progress []jobs.Progress
	mu       sync.Mutex
}

const (
	testID     = 192
	testWork   = "johnny-crows-garden-brooke"
	testRec    = "johnny-crows-garden-brooke-librivox-192"
	testItem   = "johnny_crow_garden_librivox"
	testAuthor = "L. Leslie Brooke"
	testTitle  = "Johnny Crow's Garden"
)

func sectionURL(n int) string {
	return fmt.Sprintf("https://www.archive.org/download/%s/johnnycrow_%02d_brooke_64kb.mp3", testItem, n)
}

const coverURL = "https://archive.org/download/" + testItem + "/johnnycrow_1104.jpg"

// jpeg is the smallest prefix http.DetectContentType calls image/jpeg.
var jpeg = []byte("\xFF\xD8\xFFcover-bytes")

func newHarness(t *testing.T) *harness {
	t.Helper()
	x := &harness{mount: t.TempDir(), stateDir: filepath.Join(t.TempDir(), "librivox"), free: 1 << 40}
	cover := coverURL
	x.meta = &fakeMeta{
		book: metaops.LibrivoxBook{ID: strconv.Itoa(testID), Title: testTitle,
			Authors:     []metaops.LibrivoxPerson{{FirstName: "L. Leslie", LastName: "Brooke"}},
			URLIArchive: "https://www.archive.org/details/" + testItem, CoverartJPG: &cover,
			Sections: []metaops.LibrivoxSection{
				{Title: "Johnny Crow's Garden", ListenURL: sectionURL(1)},
				{Title: "Johnny Crow's Party", ListenURL: sectionURL(2)},
				{Title: "Johnny Crow's New Garden", ListenURL: sectionURL(3)},
			}},
		work: metaops.LibrivoxWork{Title: testTitle, Authors: []string{testAuthor},
			Recordings: []metaops.LibrivoxRecording{{ID: testRec, LibrivoxID: testID, ArchiveID: testItem}}},
	}
	x.fetch = &fakeFetcher{files: map[string]*remote{
		sectionURL(1): {body: mp3('1', 1000), etag: `"etag-1"`},
		sectionURL(2): {body: mp3('2', 2000), etag: `"etag-2"`},
		sectionURL(3): {body: mp3('3', 3000), etag: `"etag-3"`},
		coverURL:      {body: jpeg},
	}}
	x.srv = &fakeServer{}
	x.libs = &fakeLibs{mount: x.mount, writable: true}
	x.h = New(Options{Meta: x.meta, Fetcher: x.fetch, Libraries: x.libs, StateDir: x.stateDir,
		Mount:    func(id int64, _ string) (string, bool) { return x.libs.mountOf(id), true },
		Server:   func() Server { return x.srv },
		ServerID: func() string { return x.serverID },
		Free:     func(string) (int64, error) { return x.free, nil },
		Wait:     func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		onProgress: func(p jobs.Progress) {
			x.mu.Lock()
			x.progress = append(x.progress, p)
			x.mu.Unlock()
		}})
	return x
}

func (x *harness) job(resumed bool) jobs.Job {
	lib := int64(3)
	return jobs.Job{ID: 41, Kind: Kind, LibraryID: &lib, Attempt: 1, Resumed: resumed,
		Params: []byte(fmt.Sprintf(`{"librivox_id":%d,"work_id":%q,"recording_id":%q,"library_id":3}`, testID, testWork, testRec))}
}

func (x *harness) run(ctx context.Context, resumed bool) (any, error) {
	return x.h.Run(ctx, x.job(resumed), jobs.LocalReporter())
}

func (x *harness) path(rel string) string { return filepath.Join(x.mount, filepath.FromSlash(rel)) }

func (x *harness) exists(rel string) bool {
	_, err := os.Lstat(x.path(rel))
	return err == nil
}

func (x *harness) steps() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for _, p := range x.progress {
		if len(out) == 0 || out[len(out)-1] != p.Step {
			out = append(out, p.Step)
		}
	}
	return out
}

// failureCode is the run's coded failure, "" for none.
func failureCode(t *testing.T, out any, err error) string {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		return ""
	}
	if r, ok := out.(*Failure); !ok || r != f {
		t.Fatalf("a failure's result isn't the failure: %#v", out)
	}
	return f.Code
}
