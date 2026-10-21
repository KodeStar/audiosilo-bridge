package podcast

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func phaseFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/phase2c2/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestPhase2c2Manifest(t *testing.T) {
	var m struct {
		Files map[string]struct {
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		}
	}
	if err := json.Unmarshal(phaseFixture(t, "manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	for name, item := range m.Files {
		b := phaseFixture(t, name)
		h := sha256.Sum256(b)
		if len(b) != item.Bytes || hex.EncodeToString(h[:]) != item.SHA256 {
			t.Errorf("fixture mismatch: %s", name)
		}
	}
}
func TestGenerationVectors(t *testing.T) {
	var vectors struct {
		Transitions []struct {
			Previous   *Fetch `json:"previous"`
			Received   string `json:"received_sha256"`
			Generation int    `json:"generation"`
			Changed    bool   `json:"timeline_changed"`
		}
	}
	json.Unmarshal(phaseFixture(t, "generation-vectors.json"), &vectors)
	for _, v := range vectors.Transitions {
		actual, err := NextFetch(v.Previous, Fetch{SHA256: v.Received, Bytes: 100, DurationMS: 10, FetchedAt: "2026-10-21T08:00:00Z"})
		if err != nil || actual.Generation != v.Generation || actual.TimelineChanged != v.Changed || actual.DeletedAt != "" {
			t.Fatal(actual, err, v)
		}
	}
	for _, f := range []*Fetch{nil, {}, {Generation: 1, SHA256: strings.Repeat("a", 64), Bytes: 1, DurationMS: 1, FetchedAt: "bad"}} {
		if f.Valid() {
			t.Fatalf("invalid fetch accepted: %+v", f)
		}
	}
}
func TestCHAPGoldens(t *testing.T) {
	for _, name := range []string{"chap-v23", "chap-v24-reverse"} {
		var want ChapterList
		json.Unmarshal(phaseFixture(t, name+".chapters.json"), &want)
		got := ReadCHAP(bytes.NewReader(phaseFixture(t, name+".mp3")), "Fixture Show/2026-10-19-1cc306d831.mp3", Fetch{Bytes: want.Files[0].Size, DurationMS: want.Files[0].DurationMS, FetchedAt: want.GeneratedAt})
		if !reflect.DeepEqual(got, &want) {
			t.Fatalf("%s got %#v, want %#v", name, got, want)
		}
	}
	if ReadCHAP(bytes.NewReader(phaseFixture(t, "plain.mp3")), "x.mp3", Fetch{DurationMS: 4056}) != nil {
		t.Fatal("invented chapters")
	}
	raw := phaseFixture(t, "chap-v23.mp3")
	for _, n := range []int{0, 3, 9, 20, 100} {
		if ReadCHAP(bytes.NewReader(raw[:n]), "x.mp3", Fetch{DurationMS: 4056}) != nil {
			t.Fatal("truncated tag accepted")
		}
	}
	for _, duration := range []int64{0, 500, 2500} {
		if ReadCHAP(bytes.NewReader(raw), "x.mp3", Fetch{DurationMS: duration}) != nil {
			t.Fatal("out of bounds timing")
		}
	}
	bad := bytes.Clone(raw)
	copy(bad[6:10], []byte{0x7f, 0x7f, 0x7f, 0x7f})
	if ReadCHAP(bytes.NewReader(bad), "x.mp3", Fetch{DurationMS: 4056}) != nil {
		t.Fatal("oversize tag")
	}
}
func journalFixture(t *testing.T) (*os.Root, Journal) {
	t.Helper()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Fixture Show"), 0755)
	os.WriteFile(filepath.Join(dir, "Fixture Show", "feed.json"), phaseFixture(t, "feed-fetch-v1.json"), 0644)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	var j Journal
	json.Unmarshal(phaseFixture(t, "publish-v1.json"), &j)
	f, err := Read(root, "Fixture Show")
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Episodes {
		if f.Episodes[i].Path == j.Path {
			f.Episodes[i].Fetch = nil
		}
	}
	if _, err := Write(root, "Fixture Show", f, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Fixture Show", j.AudioTemp), phaseFixture(t, "chap-v24-reverse.mp3"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, j
}
func TestPublicationFaultRecovery(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	for _, boundary := range []string{"journal", "audio", "sidecar", "feed", "remove"} {
		t.Run(boundary, func(t *testing.T) {
			root, j := journalFixture(t)
			crash := errors.New("injected stop")
			err := Publish(root, j, now, func(at string) error {
				if at == boundary {
					return crash
				}
				return nil
			})
			if !errors.Is(err, crash) {
				t.Fatal(err)
			}
			if err := RecoverShow(root, "Fixture Show", now, nil); err != nil {
				t.Fatal(err)
			}
			if !Matches(root, j.Path, &j.Fetch) {
				t.Fatal("candidate lost")
			}
			f, _ := Read(root, "Fixture Show")
			e := episodeFor(f, j)
			if e == nil || !reflect.DeepEqual(e.Fetch, &j.Fetch) {
				t.Fatal("fetch mismatch", e)
			}
			if _, err := root.Lstat(AttachmentPath(j.Path, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("journal survives", err)
			}
			j.Op = "delete"
			j.AudioTemp = ""
			j.Chapters = nil
			j.Fetch.DeletedAt = now.Add(time.Hour).Format(time.RFC3339)
			j.TrashDir = ".audiosilo-trash/podcasts/" + j.EpisodeID + "-g2-test"
			err = Publish(root, j, now, func(at string) error {
				if at == boundary {
					return crash
				}
				return nil
			})
			if !errors.Is(err, crash) {
				t.Fatal(err)
			}
			if err := RecoverShow(root, "Fixture Show", now, nil); err != nil {
				t.Fatal(err)
			}
			f, _ = Read(root, "Fixture Show")
			if e := episodeFor(f, j); e.Fetch.DeletedAt != j.Fetch.DeletedAt || e.Fetch.Codec != "mp3" {
				t.Fatal(e)
			}
			if _, err := root.Lstat(j.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("audio survives", err)
			}
		})
	}
}
func TestPublicationRefusesForeignAndSymlinkPaths(t *testing.T) {
	for _, kind := range []string{"audio", "sidecar", "journal", "show", "attachments", "temp", "traversal", "identity"} {
		t.Run(kind, func(t *testing.T) {
			root, j := journalFixture(t)
			switch kind {
			case "audio":
				root.WriteFile(j.Path, []byte("foreign"), 0644)
			case "show":
				root.Rename("Fixture Show", "Old")
				os.Symlink(filepath.Join(root.Name(), "Old"), filepath.Join(root.Name(), "Fixture Show"))
			case "attachments":
				os.Symlink(t.TempDir(), filepath.Join(root.Name(), "Fixture Show", ".audiosilo"))
			case "temp":
				root.Remove("Fixture Show/" + j.AudioTemp)
				os.Symlink(filepath.Join(root.Name(), "Fixture Show", "feed.json"), filepath.Join(root.Name(), "Fixture Show", j.AudioTemp))
			case "sidecar", "journal":
				EnsureDir(root, "Fixture Show/.audiosilo")
				suffix := ".chapters.json"
				if kind == "journal" {
					suffix = ".publish.json"
				}
				os.Symlink(filepath.Join(root.Name(), "Fixture Show", "feed.json"), filepath.Join(root.Name(), AttachmentPath(j.Path, suffix)))
			case "traversal":
				j.AudioTemp = "../outside"
			case "identity":
				j.ShowID = "1111111111111111"
			}
			if err := Publish(root, j, time.Now(), nil); err == nil {
				t.Fatal("unsafe publication allowed")
			}
		})
	}
}

func TestInvalidFetchKeepsEpisodeAndChapterTextBounds(t *testing.T) {
	var f Feed
	b := phaseFixture(t, "feed-fetch-v1.json")
	b = bytes.Replace(b, []byte(`"generation": 2`), []byte(`"generation": "bad"`), 1)
	if err := json.Unmarshal(b, &f); err != nil || len(f.Episodes) != 3 || f.Episodes[0].Fetch.Valid() {
		t.Fatal("invalid fetch lost episode", err)
	}
	for _, tc := range []struct {
		input []byte
		want  string
	}{
		{[]byte{0, 'C', 'a', 'f', 0xe9}, "Café"},
		{[]byte{1, 0xff, 0xfe, 'H', 0, 'i', 0}, "Hi"},
		{[]byte{2, 0, 'H', 0, 'i'}, "Hi"},
		{append([]byte{3}, []byte(" \nIntro\u202e ")...), "Intro"},
		{[]byte{3, 0xff}, ""},
	} {
		if got := chapterText(tc.input); got != tc.want {
			t.Fatalf("text %q want %q", got, tc.want)
		}
	}
	long := append([]byte{0}, bytes.Repeat([]byte{0xe9}, 2000)...)
	if got := chapterText(long); len(got) > 200 || !utf8.ValidString(got) {
		t.Fatal("unbounded title")
	}
	// A long title is clipped in its own encoding, never dropped for Chapter N:
	// a byte that is no UTF-8 rune start ends Latin-1 and UTF-16 text too.
	degree := strings.Repeat("\u00b0", 100)
	for _, input := range [][]byte{
		append([]byte{0}, bytes.Repeat([]byte{0xb0}, 1100)...),
		append([]byte{2}, bytes.Repeat([]byte{0, 0xb0}, 600)...),
		append([]byte{1, 0xff, 0xfe}, bytes.Repeat([]byte{0xb0, 0}, 600)...),
		append([]byte{3}, []byte(strings.Repeat("\u00b0", 600))...),
	} {
		if got := chapterText(input); got != degree {
			t.Fatalf("long title %q (%d bytes)", got, len(got))
		}
	}
}

func TestRecoveryAfterExclusiveLinkBeforeUnlink(t *testing.T) {
	root, j := journalFixture(t)
	crash := errors.New("interrupted after link")
	if err := Publish(root, j, time.Now(), func(at string) error {
		if at == "linked" {
			return crash
		}
		return nil
	}); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	if err := RecoverShow(root, "Fixture Show", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("Fixture Show/" + j.AudioTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned duplicate temp retained", err)
	}
	if !Matches(root, j.Path, &j.Fetch) {
		t.Fatal("committed audio lost")
	}
}
