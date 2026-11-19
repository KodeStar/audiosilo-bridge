package podcast

import (
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// countReads counts the feed.json parses for the rest of the test.
func countReads(t *testing.T) *int {
	t.Helper()
	n := 0
	readHook = func(string) { n++ }
	t.Cleanup(func() { readHook = nil })
	return &n
}

// A publication over the feed its caller read under the show lock parses
// feed.json no more (bridge#13: about five parses per fetch); without one it
// parses it once, where it used to read it for Publish, for complete and again
// for Write's compare.
func TestPublishParsesTheFeedOnce(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	for _, given := range []bool{true, false} {
		root, j := journalFixture(t)
		var f *Feed
		if given {
			var err error
			if f, err = Read(root, "Fixture Show"); err != nil {
				t.Fatal(err)
			}
		}
		reads := countReads(t)
		if err := Publish(root, f, j, now, nil); err != nil {
			t.Fatal(err)
		}
		want := map[bool]int{true: 0, false: 1}[given]
		if *reads != want {
			t.Fatalf("feed given %v: %d parses, want %d", given, *reads, want)
		}
		readHook = nil
		saved, err := Read(root, "Fixture Show")
		if err != nil {
			t.Fatal(err)
		}
		if e := episodeFor(saved, j); e == nil || !reflect.DeepEqual(e.Fetch, &j.Fetch) || saved.UpdatedAt != now.Format(time.RFC3339) {
			t.Fatal("the pin was not written", e, saved.UpdatedAt)
		}
	}
}

// rename moves the fixture show's folder by hand.
func rename(t *testing.T, root *os.Root, to string) {
	t.Helper()
	if err := os.Rename(root.Name()+"/Fixture Show", root.Name()+"/"+to); err != nil {
		t.Fatal(err)
	}
}

// A show folder renamed by hand still names its episodes under the old folder:
// Read refuses it, ReadMoved finds where it came from and Adopt moves every
// path (feed.json, a pending journal, a committed trash manifest) to the new
// name, so recovery and reads work there again.
func TestAdoptARenamedShowFolder(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, j := journalFixture(t)
	if err := Publish(root, nil, j, now, stopAt("journal")); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	manifest := j
	manifest.Op, manifest.AudioTemp, manifest.Chapters = "delete", "", nil
	manifest.Fetch.DeletedAt = now.Format(time.RFC3339)
	manifest.TrashDir = ".audiosilo-trash/podcasts/" + j.EpisodeID + "-g1-test"
	if err := EnsureDir(root, "Fixture Show/"+manifest.TrashDir); err != nil {
		t.Fatal(err)
	}
	putJournal(t, root, "Fixture Show/"+manifest.TrashDir+"/transaction.json", manifest)
	rename(t, root, "Renamed Show")

	if _, err := Read(root, "Renamed Show"); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("a renamed folder read as its own:", err)
	}
	if err := RecoverShow(root, "Renamed Show", now, nil); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("recovery took a journal naming another folder:", err)
	}
	f, from, err := ReadMoved(root, "Renamed Show")
	if err != nil || from != "Fixture Show" {
		t.Fatal(from, err)
	}
	if err := Adopt(root, "Renamed Show", from, f, now); err != nil {
		t.Fatal(err)
	}
	moved := "Renamed Show/" + strings.TrimPrefix(j.Path, "Fixture Show/")
	pending, err := ReadJournal(root, AttachmentPath(moved, ".publish.json"))
	if err != nil || pending.Path != moved {
		t.Fatal("pending journal", pending.Path, err)
	}
	committed, err := ReadJournal(root, "Renamed Show/"+manifest.TrashDir+"/transaction.json")
	if err != nil || committed.Path != moved {
		t.Fatal("trash manifest", committed.Path, err)
	}
	// The pending install completes under the new name.
	if err := RecoverShow(root, "Renamed Show", now, nil); err != nil {
		t.Fatal(err)
	}
	after, err := Read(root, "Renamed Show")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after.Episodes {
		if !strings.HasPrefix(e.Path, "Renamed Show/") {
			t.Fatal("an episode still names the old folder", e.Path)
		}
	}
	if e := episodeFor(after, pending); e == nil || !reflect.DeepEqual(e.Fetch, &j.Fetch) || !Matches(root, moved, &j.Fetch) {
		t.Fatal("the install did not finish under the new name", e)
	}
	if _, from, err := ReadMoved(root, "Renamed Show"); err != nil || from != "" {
		t.Fatal("an adopted show still reads as moved", from, err)
	}
}

// Adopting again after a stop part way through (the journals moved, feed.json
// not yet) leaves the moved journals alone and finishes.
func TestAdoptAgainAfterAStop(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, j := journalFixture(t)
	if err := Publish(root, nil, j, now, stopAt("journal")); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	rename(t, root, "Renamed Show")
	moved := j
	moved.Path = "Renamed Show/" + strings.TrimPrefix(j.Path, "Fixture Show/")
	putJournal(t, root, AttachmentPath(moved.Path, ".publish.json"), moved)
	f, from, err := ReadMoved(root, "Renamed Show")
	if err != nil || from != "Fixture Show" {
		t.Fatal(from, err)
	}
	if err := Adopt(root, "Renamed Show", from, f, now); err != nil {
		t.Fatal(err)
	}
	if err := RecoverShow(root, "Renamed Show", now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root, "Renamed Show"); err != nil {
		t.Fatal(err)
	}
}

// A folder whose feed.json names a folder that is still there is a copy, not
// a rename: never taken in (two folders would hold one show).
func TestReadMovedRefusesACopy(t *testing.T) {
	root, _ := journalFixture(t)
	if err := os.Mkdir(root.Name()+"/Copy", 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := root.ReadFile("Fixture Show/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("Copy/feed.json", b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMoved(root, "Copy"); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("a copy was taken for a rename:", err)
	}
	if f, from, err := ReadMoved(root, "Fixture Show"); err != nil || from != "" || f == nil {
		t.Fatal("a show in its own folder", from, err)
	}
}

// A pending journal that names neither folder fails the adoption before
// feed.json changes.
func TestAdoptRefusesAForeignJournal(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, j := journalFixture(t)
	if err := Publish(root, nil, j, now, stopAt("journal")); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	rename(t, root, "Renamed Show")
	foreign := j
	foreign.Path = "Elsewhere/" + strings.TrimPrefix(j.Path, "Fixture Show/")
	putJournal(t, root, AttachmentPath("Renamed Show/"+strings.TrimPrefix(j.Path, "Fixture Show/"), ".publish.json"), foreign)
	f, from, err := ReadMoved(root, "Renamed Show")
	if err != nil {
		t.Fatal(err)
	}
	if err := Adopt(root, "Renamed Show", from, f, now); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("a foreign journal was adopted:", err)
	}
	if _, from, err := ReadMoved(root, "Renamed Show"); err != nil || from != "Fixture Show" {
		t.Fatal("feed.json changed after a refused adoption", from, err)
	}
}

// A trash manifest that names the old folder but could not be rewritten fails
// the adoption before feed.json moves (it would name the old folder for good,
// its trash never cleaned); the next adoption finishes once it can be.
func TestAdoptStopsOnAManifestItCouldNotRewrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder its owner cannot write")
	}
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, j := journalFixture(t)
	manifest := j
	manifest.Op, manifest.AudioTemp, manifest.Chapters = "delete", "", nil
	manifest.Fetch.DeletedAt = now.Format(time.RFC3339)
	manifest.TrashDir = ".audiosilo-trash/podcasts/" + j.EpisodeID + "-g1-test"
	if err := EnsureDir(root, "Fixture Show/"+manifest.TrashDir); err != nil {
		t.Fatal(err)
	}
	putJournal(t, root, "Fixture Show/"+manifest.TrashDir+"/transaction.json", manifest)
	rename(t, root, "Renamed Show")
	locked := root.Name() + "/Renamed Show/" + manifest.TrashDir
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	f, from, err := ReadMoved(root, "Renamed Show")
	if err != nil {
		t.Fatal(err)
	}
	if err := Adopt(root, "Renamed Show", from, f, now); err == nil {
		t.Fatal("an adoption left a manifest naming the old folder")
	}
	if _, from, err := ReadMoved(root, "Renamed Show"); err != nil || from != "Fixture Show" {
		t.Fatal("feed.json moved before the manifest", from, err)
	}
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if f, from, err = ReadMoved(root, "Renamed Show"); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(root, "Renamed Show", from, f, now); err != nil {
		t.Fatal(err)
	}
	committed, err := ReadJournal(root, "Renamed Show/"+manifest.TrashDir+"/transaction.json")
	if err != nil || !strings.HasPrefix(committed.Path, "Renamed Show/") {
		t.Fatal("trash manifest", committed.Path, err)
	}
}

// EnsureDir never makes the show folder itself: a job that runs on after its
// show folder was renamed by hand must not make the old name again, where
// ReadMoved would then take the renamed folder for a copy.
func TestEnsureDirNeverMakesTheShowFolder(t *testing.T) {
	root, _ := journalFixture(t)
	rename(t, root, "Renamed Show")
	if err := EnsureDir(root, "Fixture Show/.audiosilo"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a show folder was made again:", err)
	}
	if _, err := root.Lstat("Fixture Show"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old folder came back", err)
	}
	if _, from, err := ReadMoved(root, "Renamed Show"); err != nil || from != "Fixture Show" {
		t.Fatal(from, err)
	}
	if err := EnsureDir(root, "Renamed Show/.audiosilo-trash/podcasts"); err != nil {
		t.Fatal("folders below a show folder:", err)
	}
}

func putJournal(t *testing.T, root *os.Root, name string, j Journal) {
	t.Helper()
	if err := writeJournal(root, name, j); err != nil {
		t.Fatal(err)
	}
}
