package metaops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// feed192 is meta's LibriVox-shaped answer for project 192 (extended=1,
// coverart=1), in LibriVox's own shape: string ids, numbers and playtimes.
const feed192 = `{"books":[{"id":"192","title":"Johnny Crow's Garden","description":"","url_text_source":"",
"language":"English","copyright_year":"1903","num_sections":"2","url_rss":"https://librivox.org/rss/192",
"url_zip_file":"https://archive.org/compress/johnny_crow_garden_librivox/x.zip","url_project":"",
"url_librivox":"https://librivox.org/johnny-crows-garden-by-l-leslie-brooke/","url_other":"",
"totaltime":"0:12:00","totaltimesecs":720,
"authors":[{"id":"404","first_name":"L. Leslie","last_name":"Brooke","dob":"1862","dod":"1940"}],
"coverart_jpg":"https://archive.org/download/johnny_crow_garden_librivox/johnnycrow_1104.jpg","coverart_pdf":null,"coverart_thumbnail":null,
"url_iarchive":"https://www.archive.org/details/johnny_crow_garden_librivox",
"sections":[{"id":"","section_number":"1","title":"Johnny Crow's Garden","listen_url":"https://www.archive.org/download/johnny_crow_garden_librivox/johnnycrow_01_brooke_64kb.mp3","language":"English","playtime":"300","file_name":null,"readers":[]},
{"id":"","section_number":"2","title":"Johnny Crow's Party","listen_url":"https://www.archive.org/download/johnny_crow_garden_librivox/johnnycrow_02_brooke_64kb.mp3","language":"English","playtime":"420","file_name":null,"readers":[]}],
"genres":[],"translators":[]}]}`

const work192 = `{"id":"johnny-crows-garden-brooke","title":"Johnny Crow's Garden","authors":[{"id":"l-leslie-brooke","name":"L. Leslie Brooke"}],
"language":"en","series":[],"recordings":[
{"id":"johnny-crows-garden-brooke-librivox-192","narrators":[],"asin":[],"isbn":[],"chapter_count":2,"librivox_id":192,"archive_id":"johnny_crow_garden_librivox"},
{"id":"johnny-crows-garden-brooke-audible","narrators":[],"asin":[],"isbn":[],"chapter_count":0}]}`

func TestLibrivoxReads(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.String())
		switch {
		case r.URL.Path == "/api/v1/librivox/audiobooks" && r.URL.Query().Get("id") == "192":
			_, _ = w.Write([]byte(feed192))
		case r.URL.Path == "/api/v1/librivox/audiobooks" && r.URL.Query().Get("id") == "500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.URL.Path == "/api/v1/librivox/audiobooks":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Audiobooks could not be found"}`))
		case r.URL.Path == "/api/v1/works/johnny-crows-garden-brooke":
			_, _ = w.Write([]byte(work192))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL + "/")
	ctx := context.Background()

	b, err := c.LibrivoxAudiobook(ctx, 192)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "192" || b.Title != "Johnny Crow's Garden" || len(b.Sections) != 2 || b.CoverartJPG == nil ||
		b.Sections[1].ListenURL != "https://www.archive.org/download/johnny_crow_garden_librivox/johnnycrow_02_brooke_64kb.mp3" ||
		b.Sections[0].SectionNumber != "1" || b.Sections[1].Playtime != "420" {
		t.Fatalf("%+v", b)
	}
	if b.Authors[0].Name() != "L. Leslie Brooke" || b.ArchiveID() != "johnny_crow_garden_librivox" {
		t.Fatalf("author %q item %q", b.Authors[0].Name(), b.ArchiveID())
	}
	if queries[0] != "/api/v1/librivox/audiobooks?coverart=1&extended=1&id=192" {
		t.Fatalf("asked %s", queries[0])
	}
	if _, err := c.LibrivoxAudiobook(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := c.LibrivoxAudiobook(ctx, 500); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a 500: %v", err)
	}

	w, err := c.LibrivoxWork(ctx, "johnny-crows-garden-brooke")
	if err != nil {
		t.Fatal(err)
	}
	if w.Title != "Johnny Crow's Garden" || len(w.Authors) != 1 || w.Authors[0] != "L. Leslie Brooke" || w.LibrivoxCount() != 1 ||
		w.Recordings[0].ArchiveID != "johnny_crow_garden_librivox" {
		t.Fatalf("%+v", w)
	}
	if _, err := c.LibrivoxWork(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing work: %v", err)
	}

	off := NewClient("")
	if _, err := off.LibrivoxAudiobook(ctx, 192); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := off.LibrivoxWork(ctx, "x"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	down := NewClient("http://127.0.0.1:1")
	if _, err := down.LibrivoxAudiobook(ctx, 192); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestLibrivoxArchiveID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.archive.org/details/x_librivox": "x_librivox",
		"https://archive.org/details/x.y-z/files":    "x.y-z",
		"https://librivox.org/not-an-item/":          "",
		"":                                           "",
		"https://www.archive.org/details/":           "",
		"https://www.archive.org/details/a?b=c#d":    "a",
	} {
		if got := (LibrivoxBook{URLIArchive: raw}).ArchiveID(); got != want {
			t.Errorf("ArchiveID(%q) = %q, want %q", raw, got, want)
		}
	}
}
