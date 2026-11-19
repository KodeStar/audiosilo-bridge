package podcast

import (
	"encoding/json"
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

// A folder whose feed.json names a folder that still holds the same show is a
// copy, not a rename: never taken in (two folders would hold one show).
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
	// An old-name folder whose feed.json cannot be read may be the show's: refused.
	if err := root.WriteFile("Fixture Show/feed.json", []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMoved(root, "Copy"); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("a folder naming an unreadable show was taken for a rename:", err)
	}
}

// The old name of a folder renamed by hand may hold another show by now: two
// show folders' names swapped by hand, another show moved there, or a folder
// with no show in it. Each is a rename, taken in (the old name does not hold
// the same show).
func TestReadMovedTakesARenameWhoseOldNameHoldsAnotherShow(t *testing.T) {
	for _, swap := range []bool{true, false} {
		root, _ := journalFixture(t)
		rename(t, root, "Renamed Show")
		if err := os.Mkdir(root.Name()+"/Fixture Show", 0o755); err != nil {
			t.Fatal(err)
		}
		if swap {
			f, err := readFile(root, "Renamed Show")
			if err != nil {
				t.Fatal(err)
			}
			f.ShowID = "0123456789abcdef"
			for i := range f.Episodes {
				f.Episodes[i].Path = "Renamed Show/" + strings.TrimPrefix(f.Episodes[i].Path, "Fixture Show/")
			}
			if err := save(root, "Fixture Show", f, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		f, from, err := ReadMoved(root, "Renamed Show")
		if err != nil || from != "Fixture Show" || f.ShowID == "0123456789abcdef" {
			t.Fatal("swapped", swap, from, err)
		}
		if swap {
			other, from, err := ReadMoved(root, "Fixture Show")
			if err != nil || from != "Renamed Show" || other.ShowID != "0123456789abcdef" {
				t.Fatal("the other side of the swap", from, err)
			}
		}
	}
}

// A show folder that lost its feed.json but still holds the show's audio or a
// partial of the Bridge's is the show's own: a copy of it elsewhere (a backup)
// names it as its old folder and must not be taken in, or the show would
// move to the copy and leave the audio and the downloads behind. Audio the
// copy's feed.json does not name counts too (an episode that joined the feed
// after the backup was made, or one a normalising filesystem spells
// otherwise). A new empty folder at the old name after a rename by hand is
// still a rename, as is one holding only what is no audio.
func TestReadMovedRefusesACopyOfAFolderThatLostItsFeed(t *testing.T) {
	for _, left := range []string{"", "notes", "episode", "later episode", "partial"} {
		root, j := journalFixture(t)
		if err := os.CopyFS(root.Name()+"/Backup", os.DirFS(root.Name()+"/Fixture Show")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(root.Name() + "/Fixture Show"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root.Name()+"/Fixture Show", 0o755); err != nil {
			t.Fatal(err)
		}
		switch left {
		case "notes":
			if err := root.WriteFile("Fixture Show/notes.txt", []byte("notes"), 0o644); err != nil {
				t.Fatal(err)
			}
		case "episode":
			if err := root.WriteFile(j.Path, []byte("audio"), 0o644); err != nil {
				t.Fatal(err)
			}
		case "later episode":
			if err := root.WriteFile("Fixture Show/An Episode Published Since.MP3", []byte("audio"), 0o644); err != nil {
				t.Fatal(err)
			}
		case "partial":
			if err := root.WriteFile("Fixture Show/"+TempPrefix+"0123456789abcdef01234567", []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, from, err := ReadMoved(root, "Backup")
		if left == "" || left == "notes" {
			if err != nil || from != "Fixture Show" {
				t.Fatal("a rename with a folder holding no audio at its old name was refused", left, from, err)
			}
			continue
		}
		// ErrHeld, so the job can say why in its log, and an ErrInvalidFile still.
		// It names the folder that holds them, the one to act on.
		if !errors.Is(err, ErrHeld) || !errors.Is(err, ErrInvalidFile) || from != "Fixture Show" {
			t.Fatal("a copy was taken in while the show's folder still holds its", left, from, err)
		}
	}
}

// A feed.json over the reader's cap is too large (ErrTooLarge), not an
// invalid file a rename would explain.
func TestReadRefusesAnOversizedFeedAsTooLarge(t *testing.T) {
	root, _ := journalFixture(t)
	if err := os.Truncate(root.Name()+"/Fixture Show/feed.json", maxReadBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root, "Fixture Show"); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, _, err := ReadMoved(root, "Fixture Show"); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

// A folder renamed again by hand after an adoption that moved its journals
// but stopped before feed.json: feed.json names the first name, the journals
// the second, both gone. They are the show's (its id, an episode's id and file
// name), so the adoption moves them too, and the trash a manifest names is
// cleaned up under the new name rather than left for good.
func TestAdoptTakesJournalsOfAGoneEarlierName(t *testing.T) {
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
	rename(t, root, "Second Name")
	base := strings.TrimPrefix(j.Path, "Fixture Show/")
	second := j
	second.Path = "Second Name/" + base
	putJournal(t, root, AttachmentPath(second.Path, ".publish.json"), second)
	manifest.Path = second.Path
	putJournal(t, root, "Second Name/"+manifest.TrashDir+"/transaction.json", manifest)
	if err := os.Rename(root.Name()+"/Second Name", root.Name()+"/Third Name"); err != nil {
		t.Fatal(err)
	}
	f, from, err := ReadMoved(root, "Third Name")
	if err != nil || from != "Fixture Show" {
		t.Fatal(from, err)
	}
	if err := Adopt(root, "Third Name", from, f, now); err != nil {
		t.Fatal(err)
	}
	moved := "Third Name/" + base
	if pending, err := ReadJournal(root, AttachmentPath(moved, ".publish.json")); err != nil || pending.Path != moved {
		t.Fatal("pending journal", pending.Path, err)
	}
	committed, err := ReadJournal(root, "Third Name/"+manifest.TrashDir+"/transaction.json")
	if err != nil || committed.Path != moved {
		t.Fatal("trash manifest", committed.Path, err)
	}
	if err := RecoverShow(root, "Third Name", now, nil); err != nil {
		t.Fatal(err)
	}
	if err := CleanupTrash(root, committed); err != nil {
		t.Fatal("the trash of an earlier name was not cleaned:", err)
	}
	if _, err := root.Lstat("Third Name/" + manifest.TrashDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the trash was left", err)
	}
}

// A pending journal that is not the show's fails the adoption before
// feed.json changes: another show's (a gone folder, but another show id), one
// naming a folder that is still there, or one naming no episode of the show.
func TestAdoptRefusesAForeignJournal(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	for _, c := range []string{"show id", "folder there", "episode"} {
		root, j := journalFixture(t)
		if err := Publish(root, nil, j, now, stopAt("journal")); !errors.Is(err, errStop) {
			t.Fatal(err)
		}
		rename(t, root, "Renamed Show")
		base := strings.TrimPrefix(j.Path, "Fixture Show/")
		foreign := j
		foreign.Path = "Elsewhere/" + base
		switch c {
		case "show id":
			foreign.ShowID = "0123456789abcdef"
		case "folder there":
			if err := os.Mkdir(root.Name()+"/Elsewhere", 0o755); err != nil {
				t.Fatal(err)
			}
		case "episode":
			foreign.EpisodeID = "0123456789abcdef"
		}
		putJournal(t, root, AttachmentPath("Renamed Show/"+base, ".publish.json"), foreign)
		f, from, err := ReadMoved(root, "Renamed Show")
		if err != nil {
			t.Fatal(c, err)
		}
		if err := Adopt(root, "Renamed Show", from, f, now); !errors.Is(err, ErrInvalidFile) {
			t.Fatal(c, ": a foreign journal was adopted:", err)
		}
		if _, from, err := ReadMoved(root, "Renamed Show"); err != nil || from != "Fixture Show" {
			t.Fatal(c, ": feed.json changed after a refused adoption", from, err)
		}
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

// caseInsensitive skips t on a filesystem that tells names apart by case
// (Linux CI): the case-only rename it tests cannot happen there.
func caseInsensitive(t *testing.T, root *os.Root) {
	t.Helper()
	if _, err := root.Lstat("fixture show"); err != nil {
		t.Skip("the filesystem is case-sensitive")
	}
}

// A show folder renamed by hand only in case ("Fixture Show" to "fixture
// show") on a case-insensitive filesystem (APFS, SMB): the old name still
// opens the folder, so it is no copy. ReadMoved takes it as a rename under its
// spelling on disk, never under the old one (which would adopt it back), and
// RealName answers that spelling for either. (The old spelling still reads as
// the folder's own: the jobs resolve the name on disk first, podcastjob.)
func TestACaseOnlyRenameIsTakenIn(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, _ := journalFixture(t)
	if err := os.Rename(root.Name()+"/Fixture Show", root.Name()+"/fixture show"); err != nil {
		t.Fatal(err)
	}
	caseInsensitive(t, root)
	for _, name := range []string{"Fixture Show", "fixture show"} {
		if real, err := RealName(root, name); err != nil || real != "fixture show" {
			t.Fatal(name, real, err)
		}
	}
	f, from, err := ReadMoved(root, "fixture show")
	if err != nil || from != "Fixture Show" {
		t.Fatal(from, err)
	}
	if err := Adopt(root, "fixture show", from, f, now); err != nil {
		t.Fatal(err)
	}
	after, err := Read(root, "fixture show")
	if err != nil || !strings.HasPrefix(after.Episodes[0].Path, "fixture show/") {
		t.Fatal(err)
	}
}

// The show folder names a normal-form test spells in NFC (as the Bridge
// names folders, naming.go) and in NFD (as a Mac tool may write one).
const (
	nfcShow = "Caf\u00e9 Show"
	nfdShow = "Cafe\u0301 Show"
)

// A show folder whose name on disk differs from its feed's only in Unicode
// normal form (NFC and NFD, on a normalising filesystem such as APFS or SMB)
// was not renamed: RealName keeps the spelling it is asked for, ReadMoved
// answers no old name (the caller works under the feed's spelling,
// FeedFolder), and feed.json is never rewritten, as the server compares its
// paths byte for byte. A case-only rename still is one (above).
func TestANormalFormOnlyDifferenceIsNoRename(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, _ := journalFixture(t)
	if err := os.Rename(root.Name()+"/Fixture Show", root.Name()+"/"+nfcShow); err != nil {
		t.Fatal(err)
	}
	f, from, err := ReadMoved(root, nfcShow)
	if err != nil || from != "Fixture Show" {
		t.Fatal(from, err)
	}
	if err := Adopt(root, nfcShow, from, f, now); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(root.Name() + "/" + nfcShow + "/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root.Name()+"/"+nfcShow, root.Name()+"/"+nfdShow); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(nfcShow); err != nil {
		t.Skip("the filesystem does not normalise names")
	}
	if real, err := RealName(root, nfcShow); err != nil || real != nfcShow {
		t.Fatalf("RealName of the feed's spelling: %q %v", real, err)
	}
	if real, err := RealName(root, nfdShow); err != nil || real != nfdShow {
		t.Fatalf("RealName of the spelling on disk: %q %v", real, err)
	}
	for _, show := range []string{nfcShow, nfdShow} {
		f, from, err := ReadMoved(root, show)
		if err != nil || from != "" || FeedFolder(f, show) != nfcShow {
			t.Fatalf("%+q read as a rename: %q %v", show, from, err)
		}
	}
	if f, err := Read(root, nfcShow); err != nil || !strings.HasPrefix(f.Episodes[0].Path, nfcShow+"/") {
		t.Fatal("the feed's spelling no longer reads its folder", err)
	}
	if after, err := os.ReadFile(root.Name() + "/" + nfcShow + "/feed.json"); err != nil || string(after) != string(before) {
		t.Fatal("feed.json was rewritten", err)
	}
}

// SameName tells one name in two normal forms from two names; FeedFolder
// answers the feed's spelling only for the first.
func TestSameNameAndFeedFolder(t *testing.T) {
	if !SameName(nfcShow, nfdShow) || !SameName(nfcShow, nfcShow) || SameName(nfcShow, "caf\u00e9 show") || SameName("Show", "show") {
		t.Fatal("SameName")
	}
	f := &Feed{Episodes: []Episode{{Path: nfcShow + "/a.mp3"}}}
	if FeedFolder(f, nfdShow) != nfcShow || FeedFolder(f, "caf\u00e9 show") != "caf\u00e9 show" || FeedFolder(&Feed{}, nfdShow) != nfdShow {
		t.Fatal("FeedFolder")
	}
}

// A take-in that moved the journals to one spelling and stopped before
// feed.json, then a second case-only rename by hand: the journal names the
// folder under a spelling that is neither feed.json's nor the one on disk,
// and still opens it. It is the folder's own, and moves with the take-in
// rather than refuse it for good.
func TestACaseOnlyRenameTakesInAJournalOfAnEarlierSpelling(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, j := journalFixture(t)
	if err := Publish(root, nil, j, now, stopAt("journal")); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	earlier := j
	earlier.Path = "FIXTURE SHOW/" + strings.TrimPrefix(j.Path, "Fixture Show/")
	putJournal(t, root, AttachmentPath(j.Path, ".publish.json"), earlier)
	if err := os.Rename(root.Name()+"/Fixture Show", root.Name()+"/fixture show"); err != nil {
		t.Fatal(err)
	}
	caseInsensitive(t, root)
	f, from, err := ReadMoved(root, "fixture show")
	if err != nil || from != "Fixture Show" {
		t.Fatal(from, err)
	}
	if err := Adopt(root, "fixture show", from, f, now); err != nil {
		t.Fatal(err)
	}
	at := "fixture show/" + strings.TrimPrefix(j.Path, "Fixture Show/")
	if got, err := ReadJournal(root, AttachmentPath(at, ".publish.json")); err != nil || got.Path != at {
		t.Fatal(got.Path, err)
	}
	if err := RecoverShow(root, "fixture show", now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(AttachmentPath(at, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the journal was left", err)
	}
}

// RealName on a filesystem that tells names apart: the name as given, and the
// Lstat's error for a name that is not there.
func TestRealNameKeepsAnExactName(t *testing.T) {
	root, _ := journalFixture(t)
	if real, err := RealName(root, "Fixture Show"); err != nil || real != "Fixture Show" {
		t.Fatal(real, err)
	}
	if real, err := RealName(root, "Fixture Show/feed.json"); err != nil || real != "Fixture Show/feed.json" {
		t.Fatal(real, err)
	}
	if _, err := RealName(root, "Gone Show"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// A feed.json just under the cap in a folder renamed by hand to a longer
// name: every episode path grows with the name, but the cap is measured
// without it, so the take-in still writes it (it used to fail ErrTooLarge
// for good) and so does the next write. Past the reader's own cap it is
// still too large.
func TestALongerNameNeverMakesAShowUnadoptable(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	root, _ := journalFixture(t)
	f, err := Read(root, "Fixture Show")
	if err != nil {
		t.Fatal(err)
	}
	f.Show.Description = strings.Repeat("x", MaxFileBytes)
	b, _ := json.MarshalIndent(f, "", "  ")
	f.Show.Description = f.Show.Description[:MaxFileBytes-(len(b)+1-MaxFileBytes)-8]
	if _, err := Write(root, "Fixture Show", f, now); err != nil {
		t.Fatal(err)
	}
	long := "Fixture Show With A Much Longer Name Given By Hand"
	rename(t, root, long)
	moved, from, err := ReadMoved(root, long)
	if err != nil {
		t.Fatal(err)
	}
	if err := Adopt(root, long, from, moved, now); err != nil {
		t.Fatal("a longer name made the show unadoptable:", err)
	}
	after, err := Read(root, long)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := root.Stat(long + "/feed.json"); st.Size() <= MaxFileBytes {
		t.Fatal("the test did not cross the cap", st.Size())
	}
	after.Episodes[0].Title += " (edited)"
	if _, err := Write(root, long, after, now.Add(time.Hour)); err != nil {
		t.Fatal("the next write failed:", err)
	}
	after.Show.Description = strings.Repeat("x", maxReadBytes)
	if _, err := Write(root, long, after, now.Add(2*time.Hour)); !errors.Is(err, ErrTooLarge) {
		t.Fatal("past the reader's cap:", err)
	}
}

// A take-in that moved a pending journal to the folder's new name and stopped
// before feed.json, then the folder renamed back by hand: feed.json names the
// folder again, the journal the gone middle name. RecoverShow resolves it to
// the show's folder and finishes the install, rather than refusing the show
// for good. A journal naming a middle folder that is still there, or another
// show's, still refuses.
func TestRecoverShowResolvesAJournalOfAGoneMiddleName(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		edit  func(*Journal)
		there bool
		ok    bool
	}{
		{"gone", nil, false, true},
		{"still there", nil, true, false},
		{"another show", func(j *Journal) { j.ShowID = "0123456789abcdef" }, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, j := journalFixture(t)
			if err := Publish(root, nil, j, now, stopAt("journal")); !errors.Is(err, errStop) {
				t.Fatal(err)
			}
			middle := j
			middle.Path = "Middle Name/" + strings.TrimPrefix(j.Path, "Fixture Show/")
			if c.edit != nil {
				c.edit(&middle)
			}
			putJournal(t, root, AttachmentPath(j.Path, ".publish.json"), middle)
			if c.there {
				if err := os.Mkdir(root.Name()+"/Middle Name", 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err := RecoverShow(root, "Fixture Show", now, nil)
			if !c.ok {
				if !errors.Is(err, ErrInvalidFile) {
					t.Fatal("refusal:", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f, err := Read(root, "Fixture Show")
			if err != nil {
				t.Fatal(err)
			}
			if e := episodeFor(f, j); e == nil || !reflect.DeepEqual(e.Fetch, &j.Fetch) || !Matches(root, j.Path, &j.Fetch) {
				t.Fatal("the install did not finish in the show's folder", e)
			}
			if _, err := root.Lstat(AttachmentPath(j.Path, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the journal was left", err)
			}
		})
	}
}

// A journal naming a gone folder is resolved against the show's feed.json.
// When that file cannot be read for it (over the read cap, or a read the
// library fails just now) the show is not an invalid one: RecoverShow fails
// with the read's error, the journal is left as it is, and the next recovery
// finishes the install once the file reads again.
func TestRecoverShowKeepsAFeedReadErrorPastAJournalOfAGoneFolder(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		damage func(t *testing.T, feed string)
		want   error
	}{
		{"too large", func(t *testing.T, feed string) {
			if err := os.Truncate(feed, maxReadBytes+1); err != nil {
				t.Fatal(err)
			}
		}, ErrTooLarge},
		{"unreadable", func(t *testing.T, feed string) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("the file cannot be made unreadable here")
			}
			if err := os.Chmod(feed, 0); err != nil {
				t.Fatal(err)
			}
		}, os.ErrPermission},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, j := journalFixture(t)
			if err := Publish(root, nil, j, now, stopAt("journal")); !errors.Is(err, errStop) {
				t.Fatal(err)
			}
			middle := j
			middle.Path = "Middle Name/" + strings.TrimPrefix(j.Path, "Fixture Show/")
			name := AttachmentPath(j.Path, ".publish.json")
			putJournal(t, root, name, middle)
			feed := root.Name() + "/Fixture Show/feed.json"
			saved, err := os.ReadFile(feed)
			if err != nil {
				t.Fatal(err)
			}
			c.damage(t, feed)
			if err := RecoverShow(root, "Fixture Show", now, nil); !errors.Is(err, c.want) || errors.Is(err, ErrInvalidFile) {
				t.Fatal("a feed.json that could not be read made the show invalid:", err)
			}
			if got, err := ReadJournal(root, name); err != nil || got.Path != middle.Path {
				t.Fatal("the journal did not stay as it was", got.Path, err)
			}
			if err := os.Chmod(feed, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(feed, saved, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := RecoverShow(root, "Fixture Show", now, nil); err != nil {
				t.Fatal(err)
			}
			if !Matches(root, j.Path, &j.Fetch) {
				t.Fatal("the install did not finish once feed.json read again")
			}
			if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the journal was left", err)
			}
		})
	}
}
