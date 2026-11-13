package librivoxjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
)

const bookDir = "L. Leslie Brooke/Johnny Crow's Garden"

var stagingDir = "L. Leslie Brooke/" + stagingName(testID)

func TestFetchPlacesBook(t *testing.T) {
	x := newHarness(t)
	out, err := x.run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	res, ok := out.(*Result)
	if !ok {
		t.Fatalf("result %#v", out)
	}
	want := Result{LibraryID: 3, Path: bookDir, LibrivoxID: testID, Files: 4, Bytes: 1000 + 2000 + 3000 + int64(len(jpeg))}
	if *res != want {
		t.Fatalf("result %+v, want %+v", *res, want)
	}
	b, _ := json.Marshal(res)
	if string(b) != `{"library_id":3,"path":"L. Leslie Brooke/Johnny Crow's Garden","librivox_id":192,"files":4,"bytes":6014}` {
		t.Fatalf("result JSON %s", b)
	}
	for i, name := range []string{"01 - Johnny Crow's Garden.mp3", "02 - Johnny Crow's Party.mp3", "03 - Johnny Crow's New Garden.mp3"} {
		got, err := os.ReadFile(x.path(path.Join(bookDir, name)))
		if err != nil || !bytes.Equal(got, x.fetch.files[sectionURL(i+1)].body) {
			t.Fatalf("%s: %v", name, err)
		}
		if st, _ := os.Stat(x.path(path.Join(bookDir, name))); st.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v", name, st.Mode().Perm())
		}
	}
	if got, _ := os.ReadFile(x.path(bookDir + "/cover.jpg")); !bytes.Equal(got, jpeg) {
		t.Fatalf("cover %q", got)
	}
	// librivox.json exactly as decision 3 has it.
	got, err := os.ReadFile(x.path(bookDir + "/.audiosilo/librivox.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"version":1,"librivox_id":192,"work_id":"johnny-crows-garden-brooke","recording_id":"johnny-crows-garden-brooke-librivox-192","archive_id":"johnny_crow_garden_librivox"}` {
		t.Fatalf("librivox.json %s", got)
	}
	if x.exists(stagingDir) {
		t.Fatal("the staging folder is left")
	}
	if entries, _ := os.ReadDir(x.stateDir); len(entries) != 0 {
		t.Fatalf("state left: %v", entries)
	}
	if !slices.Equal(x.srv.rescanned, []string{bookDir}) {
		t.Fatalf("rescanned %v", x.srv.rescanned)
	}
	if s := x.steps(); !slices.Equal(s, []string{StepManifest, StepDownload, StepCover, StepPlace, StepRescan}) {
		t.Fatalf("steps %v", s)
	}
	// The download's done/total count sections.
	var last jobs.Progress
	for _, p := range x.progress {
		if p.Step == StepDownload {
			last = p
			if p.Total != 3 || p.Done > 3 {
				t.Fatalf("download progress %+v", p)
			}
		}
	}
	if last.Done != 3 {
		t.Fatalf("last download progress %+v", last)
	}
	// Every request went through the allowlist.
	for _, r := range x.fetch.seen() {
		if !allowedRaw(r.url) {
			t.Fatalf("requested %s", r.url)
		}
	}
}

func TestFetchServerAnswersAnotherPath(t *testing.T) {
	x := newHarness(t)
	x.srv.answer = "L. Leslie Brooke/Johnny Crow's Garden (indexed)"
	out, err := x.run(context.Background(), false)
	if err != nil || out.(*Result).Path != x.srv.answer {
		t.Fatalf("%#v %v", out, err)
	}
}

func TestFetchNoCover(t *testing.T) {
	for name, set := range map[string]func(x *harness){
		"none stated":     func(x *harness) { x.meta.book.CoverartJPG = nil },
		"not on archive":  func(x *harness) { c := "https://librivox.org/cover.jpg"; x.meta.book.CoverartJPG = &c },
		"not a JPEG":      func(x *harness) { x.fetch.files[coverURL].body = []byte("<html>") },
		"archive refuses": func(x *harness) { x.fetch.files[coverURL].status = 404 },
	} {
		t.Run(name, func(t *testing.T) {
			x := newHarness(t)
			set(x)
			out, err := x.run(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			if x.exists(bookDir+"/cover.jpg") || out.(*Result).Files != 3 {
				t.Fatalf("a cover was placed: %#v", out)
			}
			for _, r := range x.fetch.seen() {
				if strings.Contains(r.url, "librivox.org") {
					t.Fatal("librivox.org was requested")
				}
			}
		})
	}
}

// The host allowlist: a manifest file off archive.org, or over http, fails
// before anything is requested or written; a redirect off archive.org fails
// the job and leaves nothing.
func TestFetchHostAllowlist(t *testing.T) {
	for name, listen := range map[string]string{
		"librivox.org":  "https://librivox.org/x.mp3",
		"http":          "http://www.archive.org/download/x/1.mp3",
		"lookalike":     "https://archive.org.example/x.mp3",
		"other port":    "https://archive.org:8080/x.mp3",
		"not a url":     "::",
		"empty":         "",
		"user and pass": "https://u:p@archive.org/x.mp3",
	} {
		t.Run(name, func(t *testing.T) {
			x := newHarness(t)
			x.meta.book.Sections[1].ListenURL = listen
			out, err := x.run(context.Background(), false)
			if code := failureCode(t, out, err); code != CodeArchiveUnavailable {
				t.Fatalf("code %q (%v)", code, err)
			}
			if len(x.fetch.seen()) != 0 {
				t.Fatalf("requested %v", x.fetch.seen())
			}
			if entries, _ := os.ReadDir(x.mount); len(entries) != 0 {
				t.Fatalf("wrote %v", entries)
			}
		})
	}
	t.Run("redirect off archive.org", func(t *testing.T) {
		x := newHarness(t)
		x.fetch.files[sectionURL(2)].redirect = "https://librivox.org/elsewhere.mp3"
		out, err := x.run(context.Background(), false)
		if code := failureCode(t, out, err); code != CodeArchiveUnavailable {
			t.Fatalf("code %q (%v)", code, err)
		}
		if strings.Contains(err.Error(), "librivox.org") || strings.Contains(err.Error(), "https://") {
			t.Fatalf("a URL in the message: %v", err)
		}
		if x.exists(stagingDir) || x.exists("L. Leslie Brooke") || x.exists(bookDir) {
			t.Fatal("left files behind")
		}
	})
	t.Run("redirect within archive.org", func(t *testing.T) {
		x := newHarness(t)
		x.fetch.files[sectionURL(2)].redirect = "https://ia800300.us.archive.org/1/items/x/2.mp3"
		if _, err := x.run(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	})
}

// A stop during a section keeps the staging folder: the next run skips the
// finished sections and resumes the partial one with Range/If-Range.
func TestFetchResumesPartial(t *testing.T) {
	x := newHarness(t)
	ctx, stop := context.WithCancel(context.Background()) // a stop: Resumes
	x.fetch.files[sectionURL(2)].cut = []int{700}
	x.fetch.files[sectionURL(2)].onCut = stop
	if _, err := x.run(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	}
	if !x.exists(stagingDir + "/01 - Johnny Crow's Garden.mp3") {
		t.Fatal("the finished section went")
	}
	part, err := os.ReadFile(x.path(stagingDir + "/02 - Johnny Crow's Party.mp3.part"))
	if err != nil || len(part) != 700 {
		t.Fatalf("partial %d bytes, %v", len(part), err)
	}
	before := len(x.fetch.seen())

	out, err := x.run(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	reqs := x.fetch.seen()[before:]
	for _, r := range reqs {
		if r.url == sectionURL(1) {
			t.Fatal("a finished section was downloaded again")
		}
	}
	if reqs[0].url != sectionURL(2) || reqs[0].offset != 700 || reqs[0].etag != `"etag-2"` {
		t.Fatalf("the resume asked %+v", reqs[0])
	}
	got, _ := os.ReadFile(x.path(bookDir + "/02 - Johnny Crow's Party.mp3"))
	if !bytes.Equal(got, x.fetch.files[sectionURL(2)].body) {
		t.Fatalf("resumed file %d bytes", len(got))
	}
	if out.(*Result).Files != 4 {
		t.Fatalf("%#v", out)
	}
}

// A partial without a strong ETag can't be continued: the section starts again.
func TestFetchRestartsWithoutStrongETag(t *testing.T) {
	x := newHarness(t)
	x.fetch.files[sectionURL(2)].etag = `W/"weak"`
	x.fetch.files[sectionURL(2)].cut = []int{500}
	out, err := x.run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(x.path(bookDir + "/02 - Johnny Crow's Party.mp3"))
	if !bytes.Equal(got, x.fetch.files[sectionURL(2)].body) || out.(*Result).Bytes != 6014 {
		t.Fatalf("restarted file %d bytes", len(got))
	}
}

// Dropped connections that add to the partial go on; three in a row that add
// nothing fail archive_unavailable and remove the staging folder.
func TestFetchDroppedConnections(t *testing.T) {
	x := newHarness(t)
	x.fetch.files[sectionURL(3)].cut = []int{1000, 1000, 500}
	if _, err := x.run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	x = newHarness(t)
	x.fetch.files[sectionURL(3)].cut = []int{0, 0, 0}
	out, err := x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeArchiveUnavailable {
		t.Fatalf("code %q (%v)", code, err)
	}
	if x.exists(stagingDir) || x.exists("L. Leslie Brooke") {
		t.Fatal("a failure left the staging folder")
	}
}

// A file archive.org doesn't have fails at once, and the staging folder goes
// (a terminal failure), the author folder this fetch made too.
func TestFetchMissingFileCleansUp(t *testing.T) {
	x := newHarness(t)
	x.fetch.files[sectionURL(3)].status = 404
	out, err := x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeArchiveUnavailable {
		t.Fatalf("code %q (%v)", code, err)
	}
	if x.exists(stagingDir) || x.exists("L. Leslie Brooke") {
		t.Fatal("left the staging or author folder")
	}
	if entries, _ := os.ReadDir(x.stateDir); len(entries) != 0 {
		t.Fatalf("state left: %v", entries)
	}
	// An author folder that was there stays.
	x = newHarness(t)
	if err := os.MkdirAll(x.path("L. Leslie Brooke/Other"), 0o755); err != nil {
		t.Fatal(err)
	}
	x.fetch.files[sectionURL(3)].status = 410
	if _, err := x.run(context.Background(), false); err == nil {
		t.Fatal("no failure")
	}
	if x.exists(stagingDir) || !x.exists("L. Leslie Brooke/Other") {
		t.Fatal("the cleanup took the wrong folders")
	}
}

// An admin's cancel removes the staging folder; a lost claim keeps it.
func TestFetchCancelAndLost(t *testing.T) {
	x := newHarness(t)
	ctx, cancel := jobs.CancelContext(context.Background())
	x.fetch.files[sectionURL(2)].cut = []int{100}
	x.fetch.files[sectionURL(2)].onCut = cancel
	if _, err := x.run(ctx, false); err == nil {
		t.Fatal("a cancelled run succeeded")
	}
	if x.exists(stagingDir) || x.exists("L. Leslie Brooke") {
		t.Fatal("a cancel left the staging folder")
	}
	if entries, _ := os.ReadDir(x.stateDir); len(entries) != 0 {
		t.Fatalf("state left: %v", entries)
	}

	x = newHarness(t)
	ctx, lose := jobs.LoseContext(context.Background())
	x.fetch.files[sectionURL(2)].cut = []int{100}
	x.fetch.files[sectionURL(2)].onCut = lose
	if _, err := x.run(ctx, false); err == nil {
		t.Fatal("a lost run succeeded")
	}
	if !x.exists(stagingDir + "/02 - Johnny Crow's Party.mp3.part") {
		t.Fatal("a lost claim dropped the partial")
	}
}

func TestAbandon(t *testing.T) {
	x := newHarness(t)
	ctx, stop := context.WithCancel(context.Background())
	x.fetch.files[sectionURL(2)].cut = []int{100}
	x.fetch.files[sectionURL(2)].onCut = stop
	_, _ = x.run(ctx, false)
	if !x.exists(stagingDir) {
		t.Fatal("no staging folder to abandon")
	}
	x.h.Abandon(jobs.AbandonContext(context.Background(), true), x.job(true))
	if !x.exists(stagingDir) {
		t.Fatal("a lost claim's Abandon removed the staging folder")
	}
	x.h.Abandon(jobs.AbandonContext(context.Background(), false), x.job(true))
	if x.exists(stagingDir) || x.exists("L. Leslie Brooke") {
		t.Fatal("Abandon left the staging folder")
	}
	if entries, _ := os.ReadDir(x.stateDir); len(entries) != 0 {
		t.Fatalf("state left: %v", entries)
	}
}

// The title's folder taken: once "<Title> (LibriVox <id>)", then exists with
// nothing written.
func TestFetchExistsFallback(t *testing.T) {
	x := newHarness(t)
	if err := os.MkdirAll(x.path(bookDir), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := x.run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if p := out.(*Result).Path; p != bookDir+" (LibriVox 192)" {
		t.Fatalf("placed at %q", p)
	}
	if entries, _ := os.ReadDir(x.path(bookDir)); len(entries) != 0 {
		t.Fatal("the existing folder was written to")
	}

	x = newHarness(t)
	for _, d := range []string{bookDir, bookDir + " (LibriVox 192)"} {
		if err := os.MkdirAll(x.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out, err = x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeExists {
		t.Fatalf("code %q (%v)", code, err)
	}
	if x.exists(stagingDir) || len(x.fetch.seen()) != 0 {
		t.Fatal("an exists failure downloaded or staged")
	}
}

// A folder that turns up at the destination during the download: the rename
// never replaces it and takes the fallback name.
func TestFetchPlaceRace(t *testing.T) {
	x := newHarness(t)
	x.fetch.files[sectionURL(3)].cut = []int{10}
	x.fetch.files[sectionURL(3)].onCut = func() { _ = os.MkdirAll(x.path(bookDir+"/theirs"), 0o755) }
	out, err := x.run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if p := out.(*Result).Path; p != bookDir+" (LibriVox 192)" {
		t.Fatalf("placed at %q", p)
	}
	if !x.exists(bookDir + "/theirs") {
		t.Fatal("the other folder was replaced")
	}
}

// An author folder that is itself a book (listed, or holding audio) is never
// placed into.
func TestFetchParentIsABook(t *testing.T) {
	x := newHarness(t)
	x.srv.books = []link.LibraryBook{{Path: "L. Leslie Brooke", Title: "Loose", Authors: []string{"Someone Else"}}}
	out, err := x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeExists {
		t.Fatalf("listed: code %q (%v)", code, err)
	}
	x = newHarness(t)
	if err := os.MkdirAll(x.path("L. Leslie Brooke"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x.path("L. Leslie Brooke/track.mp3"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeExists {
		t.Fatalf("audio: code %q (%v)", code, err)
	}
}

// The author folder the library already uses is reused as it is spelled.
func TestFetchReusesAuthorFolder(t *testing.T) {
	x := newHarness(t)
	x.srv.books = []link.LibraryBook{{Path: "L.Leslie Brooke/Ring o' Roses", Title: "Ring o' Roses", Authors: []string{"L. Leslie Brooke"}}}
	if err := os.MkdirAll(x.path("L.Leslie Brooke/Ring o' Roses"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := x.run(context.Background(), false)
	if err != nil || out.(*Result).Path != "L.Leslie Brooke/Johnny Crow's Garden" {
		t.Fatalf("%#v %v", out, err)
	}
}

// A failed rescan keeps the placed book and its state: the next run goes on
// from the rescan, downloading nothing.
func TestFetchRescanFailureResumesAtRescan(t *testing.T) {
	x := newHarness(t)
	boom := errors.New("server busy")
	x.srv.rescanErr = []error{boom, boom, boom}
	if _, err := x.run(context.Background(), false); err == nil || !strings.Contains(err.Error(), "couldn't index it") {
		t.Fatalf("first run: %v", err)
	}
	if !x.exists(bookDir + "/03 - Johnny Crow's New Garden.mp3") {
		t.Fatal("a failed rescan removed the book")
	}
	before, reads := len(x.fetch.seen()), x.meta.reads
	out, err := x.run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.fetch.seen()) != before || x.meta.reads != reads {
		t.Fatal("the retry read or downloaded again")
	}
	if r := out.(*Result); r.Path != bookDir || r.Files != 4 || r.Bytes != 6014 {
		t.Fatalf("%+v", r)
	}
}

// A folder that already holds this project (its state lost) is the book: no
// download, only the rescan.
func TestFetchFolderAlreadyHoldsProject(t *testing.T) {
	x := newHarness(t)
	if err := os.MkdirAll(x.path(bookDir+"/.audiosilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(x.path(bookDir+"/01 - a.mp3"), []byte("abc"), 0o644)
	_ = os.WriteFile(x.path(bookDir+"/.audiosilo/librivox.json"), []byte(`{"version":1,"librivox_id":192}`), 0o644)
	out, err := x.run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*Result); r.Path != bookDir || r.Files != 1 || r.Bytes != 3 || len(x.fetch.seen()) != 0 {
		t.Fatalf("%+v, %d requests", r, len(x.fetch.seen()))
	}
}

func TestFetchMetaFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(x *harness)
		code string
	}{
		{"metadata off", func(x *harness) { x.meta.bookErr = metaops.ErrDisabled }, CodeMetadataOff},
		{"no meta client", func(x *harness) { x.h.o.Meta = nil }, CodeMetadataOff},
		{"no such project", func(x *harness) { x.meta.book.ID = "1" }, CodeNotFound},
		{"no sections", func(x *harness) { x.meta.book.Sections = nil }, CodeNotFound},
		{"recording is another project", func(x *harness) { x.meta.work.Recordings[0].LibrivoxID = 5 }, CodeNotFound},
		{"meta down", func(x *harness) { x.meta.bookErr = metaops.ErrUnavailable }, ""},
		{"work unreadable", func(x *harness) { x.meta.workErr = metaops.ErrUnavailable }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			tc.set(x)
			out, err := x.run(context.Background(), false)
			if err == nil {
				t.Fatal("no failure")
			}
			if code := failureCode(t, out, err); code != tc.code {
				t.Fatalf("code %q (%v)", code, err)
			}
			if entries, _ := os.ReadDir(x.mount); len(entries) != 0 {
				t.Fatalf("wrote %v", entries)
			}
		})
	}
	// A work meta doesn't have: the project names the folder.
	x := newHarness(t)
	x.meta.workErr = metaops.ErrNotFound
	x.meta.book.Title = "Johnny Crow's Garden (version 2)"
	out, err := x.run(context.Background(), false)
	if err != nil || out.(*Result).Path != "L. Leslie Brooke/Johnny Crow's Garden (version 2)" {
		t.Fatalf("%#v %v", out, err)
	}
	b, _ := os.ReadFile(x.path("L. Leslie Brooke/Johnny Crow's Garden (version 2)/.audiosilo/librivox.json"))
	var m Marker
	if json.Unmarshal(b, &m) != nil || m.ArchiveID != testItem {
		t.Fatalf("the archive id from url_iarchive: %s", b)
	}
}

func TestFetchNotWritable(t *testing.T) {
	x := newHarness(t)
	x.libs.writable = false
	out, err := x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeNotWritable {
		t.Fatalf("code %q (%v)", code, err)
	}
	x = newHarness(t)
	x.libs.kind = "podcasts"
	out, err = x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeNotWritable {
		t.Fatalf("podcasts: code %q (%v)", code, err)
	}
}

func TestFetchDiskFull(t *testing.T) {
	x := newHarness(t)
	x.free = 100
	out, err := x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeDiskFull {
		t.Fatalf("code %q (%v)", code, err)
	}
	if x.exists(stagingDir) {
		t.Fatal("disk_full left the staging folder")
	}
}

func TestWriteFailureCodes(t *testing.T) {
	var f *Failure
	if !errors.As(writeFailure(syscall.ENOSPC, "x"), &f) || f.Code != CodeDiskFull {
		t.Fatalf("ENOSPC: %v", f)
	}
	if !errors.As(writeFailure(os.ErrPermission, "x"), &f) || f.Code != CodeNotWritable {
		t.Fatalf("permission: %v", f)
	}
	if errors.As(writeFailure(errors.New("other"), "x"), &f) {
		t.Fatal("an unknown write error was coded")
	}
}

func TestParseParams(t *testing.T) {
	good := `{"librivox_id":192,"work_id":"w","recording_id":"r","library_id":3,"extra":true}`
	if p, err := ParseParams([]byte(good)); err != nil || p.LibrivoxID != 192 || p.LibraryID != 3 {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{
		`{`, `{"librivox_id":0,"work_id":"w","recording_id":"r","library_id":3}`,
		`{"librivox_id":192,"work_id":"","recording_id":"r","library_id":3}`,
		`{"librivox_id":192,"work_id":"../x","recording_id":"r","library_id":3}`,
		`{"librivox_id":192,"work_id":"w","recording_id":"r/s","library_id":3}`,
		`{"librivox_id":192,"work_id":"w","recording_id":"r","library_id":0}`,
	} {
		if _, err := ParseParams([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	x := newHarness(t)
	other := int64(4)
	job := x.job(false)
	job.LibraryID = &other
	if _, err := x.h.Run(context.Background(), job, jobs.LocalReporter()); err == nil {
		t.Fatal("a job for another library ran")
	}
}

// A state file that doesn't name this project's staging folder is dropped,
// never followed (it decides what the cleanup removes).
func TestStateValid(t *testing.T) {
	for _, tc := range []struct {
		st state
		ok bool
	}{
		{state{LibraryID: 3, Parent: "A", Staging: "A/" + stagingName(192), Dest: "A/T"}, true},
		{state{LibraryID: 3, Parent: "", Staging: stagingName(192), Dest: "T"}, true},
		{state{LibraryID: 3, Parent: "A", Staging: "A/" + stagingName(5), Dest: "A/T"}, false},
		{state{LibraryID: 3, Parent: "A", Staging: "B/" + stagingName(192), Dest: "A/T"}, false},
		{state{LibraryID: 3, Parent: "..", Staging: "../" + stagingName(192), Dest: "../T"}, false},
		{state{LibraryID: 3, Parent: "A", Staging: "A/" + stagingName(192), Dest: "A/../T"}, false},
		{state{LibraryID: 3, Parent: "A", Staging: "A/" + stagingName(192), Dest: "B/T"}, false},
		{state{LibraryID: 0, Parent: "A", Staging: "A/" + stagingName(192), Dest: "A/T"}, false},
	} {
		if got := tc.st.valid(192); got != tc.ok {
			t.Errorf("%+v valid = %v", tc.st, got)
		}
	}
	x := newHarness(t)
	_ = os.MkdirAll(x.stateDir, 0o700)
	_ = os.WriteFile(x.h.statePath(192), []byte(`{"library_id":3,"parent":"..","staging":"../`+stagingName(192)+`","dest":"../T"}`), 0o600)
	if x.h.loadState(192) != nil {
		t.Fatal("an invalid state loaded")
	}
	if _, err := os.Stat(x.h.statePath(192)); err == nil {
		t.Fatal("an invalid state stayed")
	}
}

// A resumed job whose library isn't writable now parks not_writable and goes
// on once the mapping is back.
func TestFetchResumedParksForMapping(t *testing.T) {
	x := newHarness(t)
	x.libs.writable = false
	waits := 0
	x.h.o.Wait = func(context.Context, time.Duration) error {
		waits++
		x.libs.writable = true
		return nil
	}
	out, err := x.run(context.Background(), true)
	if err != nil || out.(*Result).Path != bookDir {
		t.Fatalf("%#v %v", out, err)
	}
	parked := false
	for _, p := range x.progress {
		if park, ok := p.Extra["park"].(map[string]any); ok && park["code"] == CodeNotWritable {
			parked = true
		}
	}
	if !parked || waits == 0 {
		t.Fatal("the resumed job didn't park")
	}
	if x.h.Kind() != "librivox.fetch" || x.h.Lane() != jobs.LaneDownload {
		t.Fatal("kind or lane")
	}
}

// archive.org's 429 and 5xx are tried again; a 206 the Bridge can't prove
// continues its partial (a 416) starts the file again.
func TestFetchTransientStatuses(t *testing.T) {
	x := newHarness(t)
	flaky := &flakyFetcher{fakeFetcher: x.fetch, statuses: map[string][]int{
		sectionURL(1): {429, 503},
		sectionURL(2): {416},
	}}
	x.h.o.Fetcher = flaky
	out, err := x.run(context.Background(), false)
	if err != nil || out.(*Result).Bytes != 6014 {
		t.Fatalf("%#v %v", out, err)
	}
	x = newHarness(t)
	x.h.o.Fetcher = &flakyFetcher{fakeFetcher: x.fetch, statuses: map[string][]int{sectionURL(1): {503, 503, 503}}}
	out, err = x.run(context.Background(), false)
	if code := failureCode(t, out, err); code != CodeArchiveUnavailable {
		t.Fatalf("code %q (%v)", code, err)
	}
}

// flakyFetcher answers a URL's queued statuses before its file.
type flakyFetcher struct {
	*fakeFetcher
	statuses map[string][]int
}

func (f *flakyFetcher) Stream(ctx context.Context, in netguard.StreamRequest) (*netguard.StreamResponse, error) {
	if q := f.statuses[in.URL]; len(q) > 0 {
		f.statuses[in.URL] = q[1:]
		return &netguard.StreamResponse{Status: q[0], Body: io.NopCloser(strings.NewReader("")), RetryAfterSeconds: 1}, nil
	}
	return f.fakeFetcher.Stream(ctx, in)
}
