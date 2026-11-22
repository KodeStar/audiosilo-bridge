package mapping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

type markerSourceFunc func(context.Context, int64) (string, error)

func (f markerSourceFunc) Marker(ctx context.Context, id int64) (string, error) { return f(ctx, id) }
func (f markerSourceFunc) Book(context.Context, int64, string) (*link.Book, error) {
	return nil, errors.New("book route must not be used")
}

func TestPodcastMarkerRoundTripAndRegistry(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	lib := link.Library{ID: 3, Root: mount, Kind: "podcasts"}
	source := markerSourceFunc(func(_ context.Context, id int64) (string, error) {
		b, err := os.ReadFile(filepath.Join(mount, MarkerPath))
		if err != nil {
			return "", err
		}
		var marker libraryMarker
		if err := json.Unmarshal(b, &marker); err != nil {
			return "", err
		}
		if marker.LibraryID != id {
			t.Fatal("wrong marker id")
		}
		return marker.Nonce, nil
	})
	st, unanswered := CheckPodcast(context.Background(), source, lib, mount, func(id int64) bool { return id == 4 })
	if !st.Mapped || !st.Writable || st.CheckedPath != MarkerPath || unanswered != nil {
		t.Fatalf("check: %+v", st)
	}
	b, _ := os.ReadFile(filepath.Join(mount, MarkerPath))
	st, _ = CheckPodcast(context.Background(), source, lib, mount, func(id int64) bool { return id == 4 })
	after, _ := os.ReadFile(filepath.Join(mount, MarkerPath))
	if !st.Mapped || string(b) != string(after) {
		t.Fatal("changed an existing marker")
	}
	r := newRegistry()
	r.SetLibraries([]link.Library{lib})
	r.Check(context.Background(), source, func(int64, string) (string, bool) { return mount, true }, time.Now)
	st, ok := r.Status(3)
	if !ok || !st.Writable {
		t.Fatalf("registry: %+v", st)
	}
}

// An unanswered marker read keeps a podcast library's last state only while
// the folder still holds its marker. With the marker gone (the share isn't
// mounted, or the folder was swapped) nothing proves that state any more, so
// the failure is recorded and podcast writes wait.
func TestPodcastUnansweredCheckNeedsTheMarker(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	lib := link.Library{ID: 3, Root: mount, Kind: "podcasts"}
	var down error
	source := markerSourceFunc(func(context.Context, int64) (string, error) {
		if down != nil {
			return "", down
		}
		b, err := os.ReadFile(filepath.Join(mount, MarkerPath))
		if err != nil {
			return "", link.ErrNoMarker
		}
		var marker libraryMarker
		if err := json.Unmarshal(b, &marker); err != nil {
			return "", err
		}
		return marker.Nonce, nil
	})
	mountFn := func(int64, string) (string, bool) { return mount, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.SetLibraries([]link.Library{lib})
	r.Check(context.Background(), source, mountFn, func() time.Time { return at })
	if st, _ := r.Status(3); !st.Mapped || !st.Writable {
		t.Fatalf("the first check = %+v", st)
	}

	down = &link.APIError{Status: 503}
	r.Check(context.Background(), source, mountFn, func() time.Time { return at })
	if st, _ := r.Status(3); !st.Mapped || !st.Writable {
		t.Fatalf("an unanswered check with the marker in place replaced the state: %+v", st)
	}

	if err := os.RemoveAll(filepath.Join(mount, ".audiosilo")); err != nil {
		t.Fatal(err)
	}
	r.Check(context.Background(), source, mountFn, func() time.Time { return at })
	st, ok := r.Status(3)
	if ok || st.Mapped || st.Writable || st.Error != "could not verify this folder with the server" {
		t.Fatalf("an unanswered check of a folder without the marker kept the old state, or reads as known: %+v, %v", st, ok)
	}
	if _, err := os.Lstat(filepath.Join(mount, ".audiosilo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker written for the failed read was left behind: %v", err)
	}
	if r.Due(at, 24*time.Hour) || !r.Due(at.Add(RetryMax), 24*time.Hour) {
		t.Fatal("the library isn't due again after the hold")
	}
}
func TestPodcastMarkerRefusalsAndCleanup(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"remote404", "mismatch", "other-library", "symlink-dir", "symlink-file", "oversize", "bad-nonce"} {
		t.Run(scenario, func(t *testing.T) {
			mount := t.TempDir()
			lib := link.Library{ID: 3, Kind: "podcasts"}
			source := markerSourceFunc(func(context.Context, int64) (string, error) { return "different", nil })
			preexisting := scenario != "remote404" && scenario != "mismatch"
			if scenario == "remote404" {
				source = func(context.Context, int64) (string, error) { return "", link.ErrNoMarker }
			}
			if preexisting {
				if scenario == "symlink-dir" {
					os.Symlink(t.TempDir(), filepath.Join(mount, ".audiosilo"))
				} else {
					os.Mkdir(filepath.Join(mount, ".audiosilo"), 0755)
					b := []byte(`{"version":1,"library_id":4,"nonce":"0123456789abcdef0123456789abcdef"}`)
					switch scenario {
					case "symlink-file":
						outside := filepath.Join(t.TempDir(), "marker")
						os.WriteFile(outside, b, 0644)
						os.Symlink(outside, filepath.Join(mount, MarkerPath))
					case "oversize":
						os.WriteFile(filepath.Join(mount, MarkerPath), []byte(strings.Repeat(" ", 4097)), 0644)
					case "bad-nonce":
						os.WriteFile(filepath.Join(mount, MarkerPath), []byte(`{"version":1,"library_id":3,"nonce":"BAD"}`), 0644)
					default:
						os.WriteFile(filepath.Join(mount, MarkerPath), b, 0644)
					}
				}
			}
			st, unanswered := CheckPodcast(context.Background(), source, lib, mount, func(id int64) bool { return id == 4 })
			if st.Mapped || st.Writable || st.Error == "" || unanswered != nil {
				t.Fatalf("allowed %s: %+v", scenario, st)
			}
			_, err := os.Lstat(filepath.Join(mount, ".audiosilo"))
			if preexisting && err != nil {
				t.Fatal("removed a preexisting folder")
			}
			if !preexisting && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed marker was not cleaned up")
			}
		})
	}
}

// A marker naming a library the server still has is refused (the mount shows
// that library's folder); once the server no longer has it (deleted, or this
// folder added again under a new id) the marker is replaced.
func TestPodcastMarkerOfARemovedLibraryIsReplaced(t *testing.T) {
	t.Parallel()
	mounts := map[int64]string{4: t.TempDir(), 5: t.TempDir()}
	os.Mkdir(filepath.Join(mounts[5], ".audiosilo"), 0755)
	stale := []byte(`{"version":1,"library_id":4,"nonce":"0123456789abcdef0123456789abcdef"}`)
	os.WriteFile(filepath.Join(mounts[5], MarkerPath), stale, 0644)
	source := markerSourceFunc(func(_ context.Context, id int64) (string, error) {
		b, err := os.ReadFile(filepath.Join(mounts[id], MarkerPath))
		if err != nil {
			return "", err
		}
		var marker libraryMarker
		err = json.Unmarshal(b, &marker)
		return marker.Nonce, err
	})
	mount := func(id int64, _ string) (string, bool) { return mounts[id], true }
	r := newRegistry()
	r.SetLibraries([]link.Library{{ID: 4, Kind: "podcasts"}, {ID: 5, Kind: "podcasts"}})
	r.Check(context.Background(), source, mount, time.Now)
	st, _ := r.Status(5)
	b, _ := os.ReadFile(filepath.Join(mounts[5], MarkerPath))
	if st.Mapped || st.Error == "" || string(b) != string(stale) {
		t.Fatalf("a listed library's marker was taken over: %+v %s", st, b)
	}
	r.SetLibraries([]link.Library{{ID: 5, Kind: "podcasts"}})
	r.Check(context.Background(), source, mount, time.Now)
	st, _ = r.Status(5)
	b, _ = os.ReadFile(filepath.Join(mounts[5], MarkerPath))
	if !st.Mapped || !st.Writable || !strings.Contains(string(b), `"library_id":5`) {
		t.Fatalf("a removed library's marker blocked the mapping: %+v %s", st, b)
	}
}

func TestPodcastMarkerErrorSplit(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			err        error
			message    string
			unanswered bool // the server gave no definite answer
		}{
			{"absent", link.ErrNoMarker, "this folder isn't the library the server sees", false},
			{"removed", &link.APIError{Status: 404, Code: "library_not_found"}, "the library was removed from the server", false},
			{"unauthorized", link.ErrUnauthorized, "could not verify this folder with the server", true},
			{"unavailable", &link.APIError{Status: 503}, "could not verify this folder with the server", true},
			{"limited", &link.APIError{Status: 429}, "could not verify this folder with the server", true},
			{"network", errors.New("https://PRIVATE_CANARY/token"), "could not verify this folder with the server", true},
		} {
			t.Run(fmt.Sprintf("%s/%t", tc.name, preexisting), func(t *testing.T) {
				mount := t.TempDir()
				marker := []byte(`{"version":1,"library_id":3,"nonce":"0123456789abcdef0123456789abcdef"}`)
				if preexisting {
					os.Mkdir(filepath.Join(mount, ".audiosilo"), 0755)
					os.WriteFile(filepath.Join(mount, MarkerPath), marker, 0644)
				}
				source := markerSourceFunc(func(context.Context, int64) (string, error) { return "", tc.err })
				st, unanswered := CheckPodcast(context.Background(), source, link.Library{ID: 3, Kind: "podcasts"}, mount, func(int64) bool { return true })
				if st.Mapped || st.Writable || st.Error != tc.message || (unanswered != nil) != tc.unanswered {
					t.Fatal(st, unanswered)
				}
				b, err := os.ReadFile(filepath.Join(mount, MarkerPath))
				if preexisting {
					if err != nil || string(b) != string(marker) {
						t.Fatal("preexisting marker changed", err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("created marker survives", err)
				}
			})
		}
	}
}

// The undo of the marker a failed check wrote is the look's own change: a
// look of the library asked for while the mount holds that undo waits on it
// and is slow, rather than read the marker the undo is about to remove (the
// server's read of it would then fail, a definite "not mapped", or pass and
// leave a mapped library with no marker). Once the undo is done a look answers
// again.
func TestPodcastLookWaitsOnItsUndo(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	lib := link.Library{ID: 7, Root: mount, Kind: "podcasts"}
	release := make(chan struct{})
	var calls atomic.Int32
	l := bounded
	l.podcast = func(op, m string, look func(string) (podcastFound, error)) (podcastFound, error) {
		n := calls.Add(1)
		bound := LookTimeout
		if n == 2 || n == 3 {
			bound = 20 * time.Millisecond // the held undo, and the look meanwhile
		}
		return podcastWithin(bound, LookKeptFor)(op+" (undo test)", m, func(m string) (podcastFound, error) {
			if n == 2 {
				<-release // the mount holds the failed check's undo
			}
			return look(m)
		})
	}
	var down atomic.Bool
	down.Store(true)
	src := markerSourceFunc(func(context.Context, int64) (string, error) {
		if down.Load() {
			return "", &link.APIError{Status: http.StatusServiceUnavailable}
		}
		b, err := os.ReadFile(filepath.Join(mount, MarkerPath))
		if err != nil {
			return "", link.ErrNoMarker
		}
		var marker libraryMarker
		if err := json.Unmarshal(b, &marker); err != nil {
			return "", err
		}
		return marker.Nonce, nil
	})
	current := func(int64) bool { return false }
	var lost unproven
	if st, unanswered := l.checkPodcast(context.Background(), src, lib, mount, current); st.Mapped || !errors.As(unanswered, &lost) {
		t.Fatalf("a check the server didn't answer = %+v, %v", st, unanswered)
	}
	if _, err := os.Stat(filepath.Join(mount, MarkerPath)); err != nil {
		t.Fatalf("the held undo already removed the marker: %v", err)
	}
	down.Store(false)
	var slow folderSlow
	if st, unanswered := l.checkPodcast(context.Background(), src, lib, mount, current); st.Mapped || !errors.As(unanswered, &slow) {
		t.Fatalf("a look while the undo is held = %+v, %v; want slow", st, unanswered)
	}
	close(release)
	waitUntil(t, "the undo's removal of the marker folder", func() bool {
		_, err := os.Lstat(filepath.Join(mount, ".audiosilo"))
		return errors.Is(err, os.ErrNotExist)
	})
	waitUntil(t, "a look that answers once the undo is done", func() bool {
		st, unanswered := l.checkPodcast(context.Background(), src, lib, mount, current)
		return st.Mapped && st.Writable && unanswered == nil
	})
}

type emptyBookLibrary struct {
	books
	markerSourceFunc
}

func (s emptyBookLibrary) Book(ctx context.Context, id int64, at string) (*link.Book, error) {
	return s.books.Book(ctx, id, at)
}

func TestEmptyBooksLibraryRequiresServerMarker(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name               string
		markerID           int64
		serverNonce        string
		bookErr            error
		mapped, asksMarker bool
	}{
		{"bare mount", 0, nonce, link.ErrNoBook, false, false},
		{"matching marker", 3, nonce, link.ErrNoBook, true, true},
		{"different nonce", 3, "another nonce", link.ErrNoBook, false, true},
		{"another library", 4, nonce, link.ErrNoBook, false, false},
		{"book read failed", 3, nonce, &link.APIError{Status: 503}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, MarkerPath)
			body := fmt.Sprintf(`{"version":1,"library_id":%d,"nonce":"%s"}`, tc.markerID, nonce)
			if tc.markerID != 0 {
				if err := os.Mkdir(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := newRegistry()
			r.SetLibraries([]link.Library{{ID: 3, Kind: "books", Root: dir}})
			asked := false
			source := emptyBookLibrary{books: books{err: tc.bookErr}, markerSourceFunc: func(_ context.Context, id int64) (string, error) {
				asked = true
				if id != 3 {
					t.Fatalf("asked for marker of library %d", id)
				}
				return tc.serverNonce, nil
			}}
			r.Check(t.Context(), source, func(int64, string) (string, bool) { return dir, true }, time.Now)
			st, _ := r.Status(3)
			if st.Mapped != tc.mapped || st.Writable != tc.mapped || asked != tc.asksMarker {
				t.Fatalf("status %+v, asked for marker %t", st, asked)
			}
			if tc.markerID == 0 {
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
					t.Fatalf("bare mount changed: %v, %v", entries, err)
				}
			} else if got, err := os.ReadFile(file); err != nil || string(got) != body {
				t.Fatalf("existing marker changed: %q, %v", got, err)
			}
		})
	}
}
