package mapping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	r := NewRegistry()
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
	r := NewRegistry()
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
	if !ok || st.Mapped || st.Writable || st.Error != "could not verify this folder with the server" {
		t.Fatalf("an unanswered check of a folder without the marker kept the old state: %+v", st)
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
	r := NewRegistry()
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
