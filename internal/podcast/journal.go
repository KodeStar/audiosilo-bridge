package podcast

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

// Journal is the recovery barrier understood by both the writer and server.
type Journal struct {
	Version   int          `json:"version"`
	Op        string       `json:"op"`
	ShowID    string       `json:"show_id"`
	EpisodeID string       `json:"episode_id"`
	Path      string       `json:"path"`
	AudioTemp string       `json:"audio_temp"`
	TrashDir  string       `json:"trash_dir"`
	Fetch     Fetch        `json:"fetch"`
	Chapters  *ChapterList `json:"chapters"`
}

// Boundary is an optional fault injection hook, called after durable boundaries.
type Boundary func(string) error

func hit(fn Boundary, s string) error {
	if fn != nil {
		return fn(s)
	}
	return nil
}

// TempPrefix names the Bridge's owned staging files in a show folder.
const TempPrefix = ".audiosilo-tmp-"

// OwnedTemp reports whether s is a bare staging name the Bridge may remove.
func OwnedTemp(s string) bool {
	return strings.HasPrefix(s, TempPrefix) && path.Base(s) == s && !strings.ContainsAny(s, "\\\x00")
}

func EnsureDir(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		p := strings.Join(parts[:i+1], "/")
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return ErrInvalidFile
		}
		st, err := root.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			if err = root.Mkdir(p, 0o755); err != nil {
				return err
			}
			err = root.Chmod(p, 0o755)
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return ErrInvalidFile
		}
	}
	return nil
}
func syncDir(root *os.Root, dir string) error {
	// Windows cannot flush a directory opened read-only. Each payload and journal
	// is flushed before publication there; the journal still covers process stops.
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func regularOrAbsent(root *os.Root, p string) error {
	f, err := OpenRegular(root, p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}
func journalValid(j *Journal, show string) bool {
	if j.Version != 1 || !ValidID(j.ShowID) || !ValidID(j.EpisodeID) || !ValidEpisodePath(j.Path) || path.Dir(j.Path) != show || !j.Fetch.Valid() {
		return false
	}
	if j.Op == "install" {
		if j.Chapters != nil && !ValidChapterList(j.Chapters, j.Path, &j.Fetch) {
			return false
		}
		return OwnedTemp(j.AudioTemp) && j.TrashDir == "" && j.Fetch.DeletedAt == ""
	}
	if j.Op == "delete" {
		prefix := ".audiosilo-trash/podcasts/" + j.EpisodeID + "-g"
		return strings.HasPrefix(j.TrashDir, prefix) && path.Clean(j.TrashDir) == j.TrashDir && len(strings.Split(j.TrashDir, "/")) == 3 && !strings.ContainsAny(j.TrashDir, "\\\x00") && j.AudioTemp == "" && j.Fetch.DeletedAt != "" && j.Chapters == nil
	}
	return false
}
func episodeFor(f *Feed, j Journal) *Episode {
	if f.ShowID != j.ShowID {
		return nil
	}
	for i := range f.Episodes {
		if f.Episodes[i].Path == j.Path && f.Episodes[i].ID == j.EpisodeID {
			return &f.Episodes[i]
		}
	}
	return nil
}

// Publish requires the caller's show lock and checked mapping. It never replaces audio.
func Publish(root *os.Root, j Journal, now time.Time, fault Boundary) error {
	show := path.Dir(j.Path)
	if !journalValid(&j, show) {
		return ErrInvalidFile
	}
	if err := EnsureDir(root, show+"/.audiosilo"); err != nil {
		return err
	}
	name := AttachmentPath(j.Path, ".publish.json")
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return ErrInvalidFile
	}
	if err := regularOrAbsent(root, AttachmentPath(j.Path, ".chapters.json")); err != nil {
		return err
	}
	f, err := Read(root, show)
	if err != nil {
		return err
	}
	e := episodeFor(f, j)
	if e == nil {
		return ErrInvalidFile
	}
	if _, err := root.Lstat(AttachmentPath(j.Path, ".chapters.json")); err == nil && ReadChapters(root, j.Path, e.Fetch) == nil {
		return ErrInvalidFile
	}
	if j.Op == "install" {
		if _, err := root.Lstat(j.Path); !errors.Is(err, os.ErrNotExist) {
			return ErrInvalidFile
		}
		if !Matches(root, show+"/"+j.AudioTemp, &j.Fetch) {
			return ErrInvalidFile
		}
	} else {
		if e.Fetch == nil || e.Fetch.DeletedAt != "" || e.Fetch.Generation != j.Fetch.Generation || e.Fetch.SHA256 != j.Fetch.SHA256 || !Matches(root, j.Path, e.Fetch) {
			return ErrInvalidFile
		}
		if err := EnsureDir(root, show+"/"+j.TrashDir); err != nil {
			return err
		}
	}
	if err := syncDir(root, show); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil || len(b) > 4<<20 {
		return ErrInvalidFile
	}
	if err := fsutil.WriteRootAtomic(root, path.Dir(name), path.Base(name), b, 0o644, nil); err != nil {
		return err
	}
	if err := syncDir(root, path.Dir(name)); err != nil {
		return err
	}
	if err := hit(fault, "journal"); err != nil {
		return err
	}
	// The payload was verified above under the same locks: complete need not hash it again.
	return complete(root, j, now, fault, true)
}

// RecoverShow finishes installs and moved deletions before any other show mutation.
// A delete whose audio has not moved is aborted: its aggregate may now be stale.
func RecoverShow(root *os.Root, show string, now time.Time, fault Boundary) error {
	if !ValidShow(show) {
		return ErrInvalidFile
	}
	dir := show + "/.audiosilo"
	st, err := root.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidFile
	}
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".publish.json") {
			continue
		}
		p := dir + "/" + e.Name()
		rd, err := OpenRegular(root, p)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(rd, (4<<20)+1))
		_ = rd.Close()
		if err != nil {
			return err
		}
		var j Journal
		if len(b) > 4<<20 || json.Unmarshal(b, &j) != nil || !journalValid(&j, show) || AttachmentPath(j.Path, ".publish.json") != p {
			return ErrInvalidFile
		}
		if j.Op == "delete" {
			// An unmoved deletion's aggregate may be stale: leave the episode installed.
			if _, err := root.Lstat(show + "/" + j.TrashDir + "/audio"); errors.Is(err, os.ErrNotExist) {
				if err := AbortDelete(root, j); err != nil {
					return err
				}
				continue
			}
		}
		if err := complete(root, j, now, fault, false); err != nil {
			return err
		}
	}
	return nil
}

// complete finishes a journaled transaction. verified means the caller has just
// checked the staged (install) or named (delete) payload under the same locks;
// without it a delete only finishes audio that already moved into trash.
func complete(root *os.Root, j Journal, now time.Time, fault Boundary, verified bool) error {
	show := path.Dir(j.Path)
	f, err := Read(root, show)
	if err != nil {
		return err
	}
	e := episodeFor(f, j)
	if e == nil {
		return ErrInvalidFile
	}
	side := AttachmentPath(j.Path, ".chapters.json")
	if err := regularOrAbsent(root, side); err != nil {
		return err
	}
	if _, err := root.Lstat(side); err == nil && ReadChapters(root, j.Path, e.Fetch) == nil && ReadChapters(root, j.Path, &j.Fetch) == nil {
		return ErrInvalidFile
	}
	if j.Op == "install" {
		temp := show + "/" + j.AudioTemp
		if !Matches(root, j.Path, &j.Fetch) {
			if _, err := root.Lstat(j.Path); !errors.Is(err, os.ErrNotExist) {
				return ErrInvalidFile
			}
			if !verified && !Matches(root, temp, &j.Fetch) {
				return ErrInvalidFile
			}
			if err := root.Chmod(temp, 0o644); err != nil {
				return err
			}
			// Never replaces: a foreign file appearing concurrently is never overwritten.
			linked, err := installAudio(root, temp, j.Path)
			if err != nil {
				return err
			}
			if err := hit(fault, "linked"); err != nil {
				return err
			}
			if linked {
				if err := root.Remove(temp); err != nil {
					return err
				}
			}
		}
		if _, err := root.Lstat(temp); err == nil {
			if !Matches(root, temp, &j.Fetch) {
				return ErrInvalidFile
			}
			if err := root.Remove(temp); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

	} else {
		if err := EnsureDir(root, show+"/"+j.TrashDir); err != nil {
			return err
		}
		trash := show + "/" + j.TrashDir + "/audio"
		if !Matches(root, trash, &j.Fetch) {
			if _, err := root.Lstat(trash); !errors.Is(err, os.ErrNotExist) {
				return ErrInvalidFile
			}
			if !verified {
				return ErrInvalidFile
			}
			if err := hit(fault, "deleting"); err != nil {
				return err
			}
			if err := root.Rename(j.Path, trash); err != nil {
				return err
			}
		} else if _, err := root.Lstat(j.Path); !errors.Is(err, os.ErrNotExist) {
			return ErrInvalidFile
		}
	}
	if err := syncDir(root, show); err != nil {
		return err
	}
	if j.Op == "delete" {
		if err := syncDir(root, show+"/"+j.TrashDir); err != nil {
			return err
		}
	}
	if err := hit(fault, "audio"); err != nil {
		return err
	}
	if j.Op == "install" && j.Chapters != nil {
		b, err := json.Marshal(j.Chapters)
		if err != nil {
			return err
		}
		if err := fsutil.WriteRootAtomic(root, path.Dir(side), path.Base(side), b, 0o644, nil); err != nil {
			return err
		}
	} else if _, err := root.Lstat(side); err == nil {
		if j.Op == "delete" {
			target := show + "/" + j.TrashDir + "/chapters.json"
			if _, err := root.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				return ErrInvalidFile
			}
			if err := root.Rename(side, target); err != nil {
				return err
			}
		} else if err := root.Remove(side); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(root, path.Dir(side)); err != nil {
		return err
	}
	if err := hit(fault, "sidecar"); err != nil {
		return err
	}
	e.Fetch = &j.Fetch
	if _, err := Write(root, show, f, now); err != nil {
		return err
	}
	if err := syncDir(root, show); err != nil {
		return err
	}
	if j.Op == "delete" {
		data, err := json.Marshal(j)
		if err != nil {
			return err
		}
		if err := fsutil.WriteRootAtomic(root, show+"/"+j.TrashDir, "transaction.json", data, 0o644, nil); err != nil {
			return err
		}
		if err := syncDir(root, show+"/"+j.TrashDir); err != nil {
			return err
		}
	}
	if err := hit(fault, "feed"); err != nil {
		return err
	}
	if err := root.Remove(AttachmentPath(j.Path, ".publish.json")); err != nil {
		return err
	}
	if err := syncDir(root, path.Dir(side)); err != nil {
		return err
	}
	return hit(fault, "remove")
}

// CleanupTrash runs after a successful rescan, or for an old owned transaction.
func CleanupTrash(root *os.Root, j Journal) error {
	show := path.Dir(j.Path)
	if j.Op != "delete" || !journalValid(&j, show) {
		return ErrInvalidFile
	}
	dir := show + "/" + j.TrashDir
	if err := EnsureDir(root, dir); err != nil {
		return err
	}
	if _, err := root.Lstat(AttachmentPath(j.Path, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
		return ErrInvalidFile
	}
	f, err := Read(root, show)
	if err != nil {
		return err
	}
	if episodeFor(f, j) == nil {
		return ErrInvalidFile
	}
	rd, err := OpenRegular(root, dir+"/transaction.json")
	if err != nil {
		return err
	}
	var saved Journal
	err = json.NewDecoder(io.LimitReader(rd, 4<<20)).Decode(&saved)
	_ = rd.Close()
	a, _ := json.Marshal(j)
	b, _ := json.Marshal(saved)
	if err != nil || string(a) != string(b) {
		return ErrInvalidFile
	}
	audio := dir + "/audio"
	if _, err := root.Lstat(audio); err == nil && !Matches(root, audio, &j.Fetch) {
		return ErrInvalidFile
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range []string{"audio", "chapters.json", "transaction.json"} {
		p := dir + "/" + name
		if err := regularOrAbsent(root, p); err != nil {
			return err
		}
		if err := root.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return root.Remove(dir)
}

// AbortDelete removes a barrier only when no payload has moved. The old pin stays
// authoritative when a fresh aggregate denies a deletion before its rename.
func AbortDelete(root *os.Root, j Journal) error {
	show := path.Dir(j.Path)
	if j.Op != "delete" || !journalValid(&j, show) || !Matches(root, j.Path, &j.Fetch) {
		return ErrInvalidFile
	}
	if _, err := root.Lstat(show + "/" + j.TrashDir + "/audio"); !errors.Is(err, os.ErrNotExist) {
		return ErrInvalidFile
	}
	// Only this transaction's own barrier is removed, never another journal.
	rd, err := OpenRegular(root, AttachmentPath(j.Path, ".publish.json"))
	if err != nil {
		return err
	}
	var saved Journal
	err = json.NewDecoder(io.LimitReader(rd, 4<<20)).Decode(&saved)
	_ = rd.Close()
	a, _ := json.Marshal(j)
	b, _ := json.Marshal(saved)
	if err != nil || string(a) != string(b) {
		return ErrInvalidFile
	}
	if err := root.Remove(AttachmentPath(j.Path, ".publish.json")); err != nil {
		return err
	}
	if err := syncDir(root, show+"/.audiosilo"); err != nil {
		return err
	}
	if err := root.Remove(show + "/" + j.TrashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
