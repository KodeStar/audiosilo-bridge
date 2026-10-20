package mapping

import (
	"context"
	"encoding/json"
	"errors"
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
	st := CheckPodcast(context.Background(), source, lib, mount)
	if !st.Mapped || !st.Writable || st.CheckedPath != MarkerPath {
		t.Fatalf("check: %+v", st)
	}
	b, _ := os.ReadFile(filepath.Join(mount, MarkerPath))
	st = CheckPodcast(context.Background(), source, lib, mount)
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
			st := CheckPodcast(context.Background(), source, lib, mount)
			if st.Mapped || st.Writable || st.Error == "" {
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
