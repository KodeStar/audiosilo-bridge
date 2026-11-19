package podcastjob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// lockedVault is a vault whose pairing token can't be read.
type lockedVault struct{ secrets.Store }

func (v lockedVault) Get(name string) (string, error) {
	if name == secrets.ServerToken {
		return "", secrets.ErrWrongKey
	}
	return v.Store.Get(name)
}

// Issue #87: an old partial no record names goes, from a mapping check's sweep
// (start) and from a retain's cleanOwned; one a running job, a record or a
// pending journal may still need stays, and so does every fresh file, link,
// file of another name and file in a folder that is no show. A record keeps
// its partial by the partial's name alone, and nothing goes while the
// pairing's token (which names the records' folder) can't be read.
func TestSweepPartialsRemovesOnlyOldUnrecordedOwnedTemps(t *testing.T) {
	const show = "Fixture Show"
	const partial = ".audiosilo-tmp-0123456789abcdef01234567"
	others := []string{
		".audiosilo-tmp-foreign",                   // not the Bridge's random part
		".audiosilo-tmp-0123456789abcdef0123456",   // one digit short
		".audiosilo-tmp-ABCDEF0123456789ABCDEF01",  // upper case (a different name on a case-insensitive disk too)
		".audiosilo-tmp-0123456789abcdef012345678", // one digit long
		"Episode.mp3",
	}
	for _, entry := range []string{"start", "retain"} {
		for _, scenario := range []string{"old", "fresh", "running", "recorded", "recorded elsewhere", "token unreadable", "journal", "link", "linked show", "no show", "unmapped"} {
			t.Run(entry+"/"+scenario, func(t *testing.T) {
				h, job := audioHarness(t)
				s := h.handler.s
				ctx := context.Background()
				root, err := os.OpenRoot(h.mount)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				old := s.o.Now().Add(-partialMaxAge - time.Hour)
				write := func(name string, at time.Time) {
					t.Helper()
					if err := root.WriteFile(name, []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := root.Chtimes(name, at, at); err != nil {
						t.Fatal(err)
					}
				}
				for _, name := range others {
					write(show+"/"+name, old)
				}
				target, at := show+"/"+partial, old
				switch scenario {
				case "fresh":
					at = s.o.Now().Add(-partialMaxAge + time.Hour)
				case "running":
					// A download running for an episode of the show.
					if err := h.db.RecordClaim(ctx, store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Attempt: 1, ServerID: "server-1"}); err != nil {
						t.Fatal(err)
					}
				case "recorded", "recorded elsewhere", "token unreadable":
					if scenario == "token unreadable" {
						if err := s.o.Vault.Set(secrets.ServerToken, "paired"); err != nil {
							t.Fatal(err)
						}
					}
					c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: partial, Offset: 4, Total: 100}
					if scenario == "recorded elsewhere" {
						// The show's name in another form than the folder's on disk
						// (and another library's id): the name still holds the file.
						c.LibraryID, c.Path = 4, "FIXTURE SHOW/Episode.mp3"
					}
					write(target, at)
					if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
						t.Fatal(err)
					}
					if scenario == "token unreadable" {
						s.o.Vault = lockedVault{s.o.Vault}
					}
				case "journal":
					if err := podcast.EnsureDir(root, show+"/.audiosilo"); err != nil {
						t.Fatal(err)
					}
					write(show+"/.audiosilo/Episode.publish.json", old)
				case "link":
					// A link at a temp name to an old file elsewhere: neither goes.
					write("victim", old)
					if err := os.Symlink("../victim", filepath.Join(h.mount, show, partial)); err != nil {
						t.Fatal(err)
					}
					target = "victim"
					// The link itself old too (its time can't be set): only its
					// being a link keeps it.
					s.o.Now = func() time.Time { return time.Now().Add(2 * partialMaxAge) }
				case "linked show":
					// A show folder that is a link (to a folder no show is in):
					// nothing under it is swept.
					if err := podcast.EnsureDir(root, "Elsewhere/Inner"); err != nil {
						t.Fatal(err)
					}
					write("Elsewhere/Inner/feed.json", old)
					write("Elsewhere/Inner/"+partial, old)
					if err := os.Symlink("Elsewhere/Inner", filepath.Join(h.mount, "Linked Show")); err != nil {
						t.Fatal(err)
					}
					target = "Elsewhere/Inner/" + partial
				case "no show":
					// A folder of the library that holds no feed.json.
					if err := podcast.EnsureDir(root, "Loose Folder"); err != nil {
						t.Fatal(err)
					}
					target = "Loose Folder/" + partial
				case "unmapped":
					h.libs.SetLibraries(nil)
				}
				if _, err := root.Lstat(target); err != nil {
					write(target, at)
				}
				if entry == "start" {
					h.handler.SweepPartials(ctx)
				} else {
					// The calling retain job runs at the show's path: its own row
					// does not keep the show busy.
					showPath := show
					if err := h.db.RecordClaim(ctx, store.ServerJob{ID: 82, Kind: RetainKind, LibraryID: job.LibraryID, Path: &showPath, Attempt: 1, ServerID: "server-1"}); err != nil {
						t.Fatal(err)
					}
					lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
					s.cleanOwned(ctx, root, lib, h.mount, show, 82)
					if scenario == "linked show" || scenario == "no show" {
						s.sweepPartials(ctx, root, lib, h.mount, []string{"Linked Show", "Loose Folder"}, 82)
					}
				}
				_, err = root.Lstat(target)
				if scenario == "old" {
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatal("an old unrecorded partial was kept", err)
					}
				} else if b, err := root.ReadFile(target); err != nil || string(b) != "keep" {
					t.Fatal("a protected file was removed or changed", err)
				}
				if scenario == "link" {
					if st, err := root.Lstat(show + "/" + partial); err != nil || st.Mode()&os.ModeSymlink == 0 {
						t.Fatal("the link at a temp name was removed", err)
					}
				}
				for _, name := range others {
					if b, err := root.ReadFile(show + "/" + name); err != nil || string(b) != "keep" {
						t.Fatal("a file of another name was touched:", name, err)
					}
				}
			})
		}
	}
}

// The sweep's pattern is the one the Bridge names its partials by, and its age
// outlasts the longest download, so a running job's file is never old.
func TestPartialSweepPatternAndAge(t *testing.T) {
	for range 20 {
		if name := podcast.TempPrefix + nonce(); !ownedTempName.MatchString(name) {
			t.Fatal("the sweep does not match a partial's name", name)
		}
	}
	if longest := EpisodeCeiling(4096<<20) + ceiling; partialMaxAge <= longest {
		t.Fatalf("partialMaxAge %v does not outlast the longest download %v", partialMaxAge, longest)
	}
}

// A whole-library sweep runs at most once per sweepEvery, however often the
// mappings are checked.
func TestSweepPartialsIsSpaced(t *testing.T) {
	h, _ := audioHarness(t)
	s := h.handler.s
	now := s.o.Now()
	s.o.Now = func() time.Time { return now }
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const partial = "Fixture Show/.audiosilo-tmp-0123456789abcdef01234567"
	leave := func() {
		t.Helper()
		if err := root.WriteFile(partial, []byte("left"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-partialMaxAge - time.Hour)
		if err := root.Chtimes(partial, old, old); err != nil {
			t.Fatal(err)
		}
	}
	swept := func() bool {
		h.handler.SweepPartials(context.Background())
		_, err := root.Lstat(partial)
		return errors.Is(err, os.ErrNotExist)
	}
	leave()
	if !swept() {
		t.Fatal("the first sweep kept an old partial")
	}
	leave()
	now = now.Add(sweepEvery - time.Minute)
	if swept() {
		t.Fatal("a second sweep ran within sweepEvery")
	}
	now = now.Add(time.Minute)
	if !swept() {
		t.Fatal("no sweep ran once sweepEvery had passed")
	}
}
