package podcastjob

import (
	"context"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// pairedServer is a link that names the server it reaches.
type pairedServer struct{ id string }

func (s pairedServer) RescanPodcast(context.Context, int64, string) (*link.PodcastRescan, error) {
	return &link.PodcastRescan{}, nil
}
func (s pairedServer) ServerID() string { return s.id }

// oneLibrary lists library 1 whatever server asks.
type oneLibrary struct{}

func (oneLibrary) Library(id int64) (link.Library, bool) { return link.Library{ID: id}, id == 1 }
func (oneLibrary) Status(int64) (link.LibraryStatus, bool) {
	return link.LibraryStatus{}, true
}
func (oneLibrary) Statuses() []link.LibraryStatus { return []link.LibraryStatus{{}} }

// A job's run keys its rows by its own server, and sees the server's link and
// libraries only while the link reaches that server: a job of server a, run
// while the Bridge is paired with b, writes nothing under b's id nor sends b
// anything (BR-LINK-RULE). A job no runner claimed works as the pairing does.
func TestJobRunKeysByItsOwnServer(t *testing.T) {
	cur := pairedServer{id: "b"}
	s := New(Options{ServerID: func() string { return cur.id }, Server: func() Server { return cur },
		Libraries: oneLibrary{}}).s
	job := jobs.Job{ID: 7}

	other := s.forJob(jobs.ClaimedFrom(job, "a"))
	if id := other.o.ServerID(); id != "a" {
		t.Fatalf("another server's job keys by %q, want its own a", id)
	}
	if other.o.Server() != nil {
		t.Fatal("another server's job was handed the new pairing's link")
	}
	if _, ok := other.o.Libraries.Library(1); ok || len(other.o.Libraries.Statuses()) != 0 {
		t.Fatal("another server's job saw the new pairing's libraries")
	}
	if other.shared != s.shared {
		t.Fatal("a job's view doesn't share the service's locks")
	}

	own := s.forJob(jobs.ClaimedFrom(job, "b"))
	if own.o.ServerID() != "b" || own.o.Server() == nil {
		t.Fatal("the pairing's own job lost its server")
	}
	if _, ok := own.o.Libraries.Library(1); !ok {
		t.Fatal("the pairing's own job lost its libraries")
	}
	if s.forJob(job) != s {
		t.Fatal("a job no runner claimed doesn't work as the pairing does")
	}
}

// The link a job's run is handed is read once: a pairing with another server
// between a check and a second read never hands server a's job the link of b
// (denied).
func TestJobRunReadsItsLinkOnce(t *testing.T) {
	reads := 0
	s := New(Options{ServerID: func() string { return "a" }, Libraries: oneLibrary{}, Server: func() Server {
		reads++
		if reads == 1 {
			return pairedServer{id: "a"}
		}
		return pairedServer{id: "b"} // paired with b since the first read
	}}).s
	got := s.forJob(jobs.ClaimedFrom(jobs.Job{ID: 7}, "a")).o.Server()
	if p, ok := got.(pairedServer); !ok || p.id != "a" {
		t.Fatalf("server a's job was handed %v, want a's link", got)
	}
	if next := s.forJob(jobs.ClaimedFrom(jobs.Job{ID: 7}, "a")).o.Server(); next != nil {
		t.Fatalf("server a's job was handed %v once paired with b", next)
	}
}
