package podcast

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

var errStop = errors.New("injected stop")

// stopAt is a Boundary that stops Publish at one boundary.
func stopAt(boundary string) Boundary {
	return func(at string) error {
		if at == boundary {
			return errStop
		}
		return nil
	}
}

// stuckInstall publishes j up to boundary, then removes the staged audio and
// any installed audio, as an older Bridge's exit (or a hand) left them.
func stuckInstall(t *testing.T, root *os.Root, j Journal, boundary string) {
	t.Helper()
	if err := Publish(root, j, time.Now(), stopAt(boundary)); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	for _, p := range []string{"Fixture Show/" + j.AudioTemp, j.Path} {
		if err := root.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func journalKept(t *testing.T, root *os.Root, j Journal) {
	t.Helper()
	saved, err := ReadJournal(root, AttachmentPath(j.Path, ".publish.json"))
	if err != nil || !sameJournal(saved, j) {
		t.Fatal("the journal was not kept", err)
	}
}

// A journal whose staged audio is gone before anything was installed is
// dropped: the show is writable again, the feed does not claim the episode,
// and the episode installs on its next fetch.
func TestRecoverAbortsAnInstallWhoseStagedAudioIsGone(t *testing.T) {
	root, j := journalFixture(t)
	staged, err := root.ReadFile("Fixture Show/" + j.AudioTemp)
	if err != nil {
		t.Fatal(err)
	}
	stuckInstall(t, root, j, "journal")
	if err := RecoverShow(root, "Fixture Show", time.Now(), nil); err != nil {
		t.Fatal("a stuck install still blocks the show:", err)
	}
	for _, gone := range []string{AttachmentPath(j.Path, ".publish.json"), j.Path, AttachmentPath(j.Path, ".chapters.json")} {
		if _, err := root.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("left behind", gone, err)
		}
	}
	f, err := Read(root, "Fixture Show")
	if err != nil {
		t.Fatal(err)
	}
	if e := episodeFor(f, j); e == nil || e.Fetch != nil {
		t.Fatal("the feed claims an episode never installed", e)
	}
	// The next fetch stages the episode again and installs it.
	if err := root.WriteFile("Fixture Show/"+j.AudioTemp, staged, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Publish(root, j, time.Now(), nil); err != nil {
		t.Fatal("the episode could not be fetched again:", err)
	}
	if !Matches(root, j.Path, &j.Fetch) {
		t.Fatal("the fetched-again episode was not installed")
	}
}

// With its staged audio in place recovery installs it, as before.
func TestRecoverStillInstallsAPresentStagedAudio(t *testing.T) {
	root, j := journalFixture(t)
	if err := Publish(root, j, time.Now(), stopAt("journal")); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	if err := AbortInstall(root, j); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("aborted an install whose staged audio is present:", err)
	}
	journalKept(t, root, j)
	if err := RecoverShow(root, "Fixture Show", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if !Matches(root, j.Path, &j.Fetch) {
		t.Fatal("recovery did not install the staged audio")
	}
}

// Past staging, each step leaves a trace; with any of them the journal is
// kept and recovery refuses, so a hand-removed installed episode is never
// silently forgotten.
func TestRecoverRefusesAnInstallPastStagingWithItsAudioGone(t *testing.T) {
	for _, tc := range []struct {
		name, boundary string
		chapters       bool
	}{
		{"chapters written", "sidecar", true},
		{"feed written", "feed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, j := journalFixture(t)
			if !tc.chapters {
				j.Chapters = nil
			}
			stuckInstall(t, root, j, tc.boundary)
			if err := RecoverShow(root, "Fixture Show", time.Now(), nil); !errors.Is(err, ErrInvalidFile) {
				t.Fatal("recovery forgot an install past staging:", err)
			}
			journalKept(t, root, j)
		})
	}
}

// A chapters attachment the episode's current pin does not own (one Publish
// would have refused) also keeps the journal.
func TestRecoverRefusesAStuckInstallBesideForeignChapters(t *testing.T) {
	root, j := journalFixture(t)
	stuckInstall(t, root, j, "journal")
	side := AttachmentPath(j.Path, ".chapters.json")
	if err := root.WriteFile(side, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecoverShow(root, "Fixture Show", time.Now(), nil); !errors.Is(err, ErrInvalidFile) {
		t.Fatal(err)
	}
	journalKept(t, root, j)
}

// Links fail closed: a linked journal is never read, a linked staged name
// (even a dangling one) is never taken as gone.
func TestRecoverStuckInstallFailsClosedOnLinks(t *testing.T) {
	t.Run("staged name", func(t *testing.T) {
		root, j := journalFixture(t)
		stuckInstall(t, root, j, "journal")
		if err := root.Symlink("elsewhere", "Fixture Show/"+j.AudioTemp); err != nil {
			t.Skip("no symlinks here:", err)
		}
		if err := RecoverShow(root, "Fixture Show", time.Now(), nil); !errors.Is(err, ErrInvalidFile) {
			t.Fatal(err)
		}
		if err := AbortInstall(root, j); !errors.Is(err, ErrInvalidFile) {
			t.Fatal(err)
		}
		journalKept(t, root, j)
	})
	t.Run("journal", func(t *testing.T) {
		root, j := journalFixture(t)
		stuckInstall(t, root, j, "journal")
		name := AttachmentPath(j.Path, ".publish.json")
		real := "Fixture Show/.audiosilo/real.json"
		b, err := root.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Rename(name, real); err != nil {
			t.Fatal(err)
		}
		if err := root.Symlink("real.json", name); err != nil {
			t.Skip("no symlinks here:", err)
		}
		if err := RecoverShow(root, "Fixture Show", time.Now(), nil); !errors.Is(err, ErrInvalidFile) {
			t.Fatal(err)
		}
		if err := AbortInstall(root, j); !errors.Is(err, ErrInvalidFile) {
			t.Fatal(err)
		}
		if st, err := root.Lstat(name); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the linked journal was removed", err)
		}
		if got, err := root.ReadFile(real); err != nil || !bytes.Equal(got, b) {
			t.Fatal("the link's target changed", err)
		}
	})
}
