package podcast

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"runtime"
	"slices"
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

// Hit calls the hook at boundary at, if there is one.
func (b Boundary) Hit(at string) error {
	if b != nil {
		return b(at)
	}
	return nil
}

// TempPrefix names the Bridge's owned staging files in a show folder.
const TempPrefix = ".audiosilo-tmp-"

// OwnedTemp reports whether s is a bare staging name the Bridge may remove.
func OwnedTemp(s string) bool {
	return strings.HasPrefix(s, TempPrefix) && path.Base(s) == s && !strings.ContainsAny(s, "\\\x00")
}

// EnsureDir makes the folders of name below its first component, a show
// folder that must be there already (a real folder, as every component): a
// show folder renamed by hand while a job ran is never made again at its old
// name, a stray folder left behind there (the job's files in it).
func EnsureDir(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		p := strings.Join(parts[:i+1], "/")
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return ErrInvalidFile
		}
		st, err := root.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && i > 0 {
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
// f is the show's feed.json as the caller read it under that lock (nil reads
// it), so the feed is parsed once per publication; Publish may change f.
func Publish(root *os.Root, f *Feed, j Journal, now time.Time, fault Boundary) error {
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
	if f == nil {
		var err error
		if f, err = Read(root, show); err != nil {
			return err
		}
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
	if err := writeJournal(root, name, j); err != nil {
		return err
	}
	if err := fault.Hit("journal"); err != nil {
		return err
	}
	// The payload was verified above under the same locks: complete need not hash it again.
	return complete(root, f, j, now, fault, true)
}

// RecoverShow finishes installs and moved deletions before any other show mutation.
// A delete whose audio has not moved is aborted: its aggregate may now be stale.
// An install whose staged and installed audio are both gone, with the feed not
// pinning it, is aborted too (abortInstall).
func RecoverShow(root *os.Root, show string, now time.Time, fault Boundary) error {
	if !ValidShow(show) {
		return ErrInvalidFile
	}
	dir := show + "/.audiosilo"
	entries, err := listFolder(root, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".publish.json") {
			continue
		}
		p := dir + "/" + e.Name()
		j, err := ReadJournal(root, p)
		if err != nil {
			return err
		}
		if !journalValid(&j, show) || AttachmentPath(j.Path, ".publish.json") != p {
			return ErrInvalidFile
		}
		if j.Op == "install" && stagedAudioGone(root, j) {
			// An older Bridge's failed Publish removed this staged audio.
			if err := abortInstall(root, j); err != nil {
				return err
			}
			slog.Warn("podcasts: an episode's staged download was gone; it will be fetched again", "show", show, "episode_id", j.EpisodeID)
			continue
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
		if err := complete(root, nil, j, now, fault, false); err != nil {
			return err
		}
	}
	return nil
}

// listFolder lists dir, a real folder (never a link); nothing when it does
// not exist.
func listFolder(root *os.Root, dir string) ([]os.DirEntry, error) {
	st, err := root.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidFile
	}
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.ReadDir(-1)
}

// Adopt takes in show, a folder renamed by hand from its old name from
// (ReadMoved answered both, and f): every path its transactions and feed.json
// still carry under from moves to show. The pending journals and committed
// trash manifests go first and feed.json last, so a stop part way through
// leaves a folder ReadMoved finds renamed again, and a journal adopted already
// is left as it is. A journal may also name a third folder that is gone: the
// folder was renamed again by hand after an adoption that moved its journals
// stopped before feed.json. It moves too when it is this show's (its show id)
// and names one of its episodes by id and file name. A pending journal that is
// not the show's fails the adoption (RecoverShow would refuse it under any
// name); a manifest that is not stays, as cleanup ignores it. The caller holds
// the show's lock and has checked the mapping.
func Adopt(root *os.Root, show, from string, f *Feed, now time.Time) error {
	if !ValidShow(show) || !ValidShow(from) || show == from || !episodesIn(f, from) {
		return ErrInvalidFile
	}
	// earlier reports whether j names an earlier name of this folder: from, or
	// a gone folder it was adopted to part way before this rename.
	earlier := func(j *Journal) bool {
		dir := path.Dir(j.Path)
		if dir == from {
			return journalValid(j, from)
		}
		if dir == show || !journalValid(j, dir) || j.ShowID != f.ShowID {
			return false
		}
		if _, err := root.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			return false
		}
		return slices.ContainsFunc(f.Episodes, func(e Episode) bool {
			return e.ID == j.EpisodeID && path.Base(e.Path) == path.Base(j.Path)
		})
	}
	move := func(name string) error {
		j, err := ReadJournal(root, name)
		if err != nil {
			return err
		}
		if journalValid(&j, show) {
			return nil
		}
		if !earlier(&j) {
			return ErrInvalidFile
		}
		j.Path = show + "/" + path.Base(j.Path)
		return writeJournal(root, name, j)
	}
	pending, err := listFolder(root, show+"/.audiosilo")
	if err != nil {
		return err
	}
	for _, e := range pending {
		if strings.HasSuffix(e.Name(), ".publish.json") {
			if err := move(show + "/.audiosilo/" + e.Name()); err != nil {
				return err
			}
		}
	}
	trash := show + "/.audiosilo-trash/podcasts"
	committed, err := listFolder(root, trash)
	if err != nil {
		return err
	}
	for _, e := range committed {
		if !e.IsDir() {
			continue
		}
		// A manifest that is not the show's (or not there) stays; one that
		// could not be rewritten fails the adoption before feed.json moves, or
		// it would name the old folder for good and its trash never go.
		err := move(trash + "/" + e.Name() + "/transaction.json")
		if err != nil && !errors.Is(err, ErrInvalidFile) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for i := range f.Episodes {
		f.Episodes[i].Path = show + "/" + path.Base(f.Episodes[i].Path)
	}
	if err := writeChanged(root, show, f, now); err != nil {
		return err
	}
	return syncDir(root, show)
}

// writeJournal writes j at name (at most 4 MiB, as ReadJournal reads it) and
// flushes its folder.
func writeJournal(root *os.Root, name string, j Journal) error {
	b, err := json.Marshal(j)
	if err != nil || len(b) > 4<<20 {
		return ErrInvalidFile
	}
	if err := fsutil.WriteRootAtomic(root, path.Dir(name), path.Base(name), b, 0o644, nil); err != nil {
		return err
	}
	return syncDir(root, path.Dir(name))
}

// ReadJournal decodes the journal at name (a regular file, no link in any
// component, at most 4 MiB as Publish writes it); ErrInvalidFile when it is
// too big or not a journal. It does not check the journal is valid.
func ReadJournal(root *os.Root, name string) (Journal, error) {
	var j Journal
	rd, err := OpenRegular(root, name)
	if err != nil {
		return j, err
	}
	defer rd.Close()
	b, err := io.ReadAll(io.LimitReader(rd, (4<<20)+1))
	if err != nil {
		return j, err
	}
	if len(b) > 4<<20 || json.Unmarshal(b, &j) != nil {
		return Journal{}, ErrInvalidFile
	}
	return j, nil
}

// sameJournal reports whether a and b encode identically.
func sameJournal(a, b Journal) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// complete finishes a journaled transaction. verified means the caller has just
// checked the staged (install) or named (delete) payload under the same locks;
// without it a delete only finishes audio that already moved into trash.
//
// f is the show's feed.json read under the same lock (nil reads it).
func complete(root *os.Root, f *Feed, j Journal, now time.Time, fault Boundary, verified bool) error {
	show := path.Dir(j.Path)
	if f == nil {
		var err error
		if f, err = Read(root, show); err != nil {
			return err
		}
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
			if err := fault.Hit("linked"); err != nil {
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
			if err := fault.Hit("deleting"); err != nil {
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
	if err := fault.Hit("audio"); err != nil {
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
	if err := fault.Hit("sidecar"); err != nil {
		return err
	}
	if e.Fetch != nil && *e.Fetch == j.Fetch {
		// Pinned already (a recovery after the feed was written): Write
		// compares, keeping updated_at.
		if _, err := Write(root, show, f, now); err != nil {
			return err
		}
	} else {
		// f was read under this lock: the new pin is the change, so it is
		// written without reading the file again to compare.
		e.Fetch = &j.Fetch
		if err := writeChanged(root, show, f, now); err != nil {
			return err
		}
	}
	if err := syncDir(root, show); err != nil {
		return err
	}
	if j.Op == "delete" {
		if err := writeJournal(root, show+"/"+j.TrashDir+"/transaction.json", j); err != nil {
			return err
		}
	}
	if err := fault.Hit("feed"); err != nil {
		return err
	}
	if err := root.Remove(AttachmentPath(j.Path, ".publish.json")); err != nil {
		return err
	}
	if err := syncDir(root, path.Dir(side)); err != nil {
		return err
	}
	return fault.Hit("remove")
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
	saved, err := ReadJournal(root, dir+"/transaction.json")
	if err != nil {
		return err
	}
	if !sameJournal(j, saved) {
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
	barrier, err := ownJournal(root, j)
	if err != nil {
		return err
	}
	// The empty trash folder goes first: a stop between the two leaves the barrier,
	// and the next recovery aborts again instead of orphaning an unowned folder.
	if err := root.Remove(show + "/" + j.TrashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.Remove(barrier); err != nil {
		return err
	}
	return syncDir(root, show+"/.audiosilo")
}

// stagedAudioGone reports whether neither an install's staged audio nor its
// target exists (neither name is anything, a link included). Otherwise
// complete decides as before: it finishes an install whose audio is in place
// and refuses anything else.
func stagedAudioGone(root *os.Root, j Journal) bool {
	if _, err := root.Lstat(path.Dir(j.Path) + "/" + j.AudioTemp); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	_, err := root.Lstat(j.Path)
	return errors.Is(err, os.ErrNotExist)
}

// abortInstall removes an install's barrier when its staged audio is gone and
// the feed does not pin this fetch. The journal records no step, so the steps
// after staging are told by the trace they leave: the audio at the episode's
// path, the chapters attachment for this fetch (or any other the episode's
// current pin does not own, which Publish would have refused), and the feed's
// pin of this fetch. With any of those present it refuses, as recovery did
// before. Two steps can run and leave nothing: audio installed and then removed
// by hand, and (a journal without chapters) the removal of the earlier pin's
// attachment. Neither matters: Publish required the episode's path empty, so the
// earlier pin had no audio there. The feed keeps the episode's earlier pin, so
// the episode stays not installed and is fetched again.
func abortInstall(root *os.Root, j Journal) error {
	show := path.Dir(j.Path)
	if j.Op != "install" || !journalValid(&j, show) {
		return ErrInvalidFile
	}
	// Read rejects a link in any component of the show's feed.json: the show
	// folder is a real folder for the absence checks below.
	f, err := Read(root, show)
	if err != nil {
		return err
	}
	e := episodeFor(f, j)
	if e == nil || (e.Fetch != nil && *e.Fetch == j.Fetch) {
		return ErrInvalidFile
	}
	if !stagedAudioGone(root, j) {
		return ErrInvalidFile
	}
	side := AttachmentPath(j.Path, ".chapters.json")
	if err := regularOrAbsent(root, side); err != nil {
		return err
	}
	if _, err := root.Lstat(side); err == nil && (ReadChapters(root, j.Path, &j.Fetch) != nil || ReadChapters(root, j.Path, e.Fetch) == nil) {
		return ErrInvalidFile
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	barrier, err := ownJournal(root, j)
	if err != nil {
		return err
	}
	if err := root.Remove(barrier); err != nil {
		return err
	}
	return syncDir(root, show+"/.audiosilo")
}

// ownJournal returns the name of j's journal when the journal there is j's own:
// an abort removes only this transaction's barrier, never another journal.
func ownJournal(root *os.Root, j Journal) (string, error) {
	name := AttachmentPath(j.Path, ".publish.json")
	saved, err := ReadJournal(root, name)
	if err != nil {
		return "", err
	}
	if !sameJournal(j, saved) {
		return "", ErrInvalidFile
	}
	return name, nil
}
