package generate

import (
	"context"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
)

type podcastLibraries struct{ Libraries }

func (podcastLibraries) Library(id int64) (link.Library, bool) {
	return link.Library{ID: id, Kind: "podcasts"}, true
}

func TestBookWritersRefusePodcastLibrary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.h.o.Libraries = podcastLibraries{h.h.o.Libraries}
	lib := int64(1)
	path := "Show/episode.mp3"
	job := jobs.Job{ID: 900, LibraryID: &lib, Path: &path, Params: []byte(`{}`)}
	for _, handler := range []jobs.Handler{h.h, h.h.DescribeJob(), h.h.ChaptersJob()} {
		job.Kind = handler.Kind()
		_, err := handler.Run(context.Background(), job, jobs.LocalReporter())
		if err == nil || !strings.Contains(err.Error(), "not_book_library") {
			t.Fatalf("%s accepted podcast library: %v", job.Kind, err)
		}
	}
}
