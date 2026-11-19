package audiblejob

import (
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/importjob"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/placement"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// titlePlanner works out, at the poll, where a backup of each unmatched title
// would go: the import's own Destination (no dest_path: the console's override
// is checked when the backup starts) over the books the poll read, spelled and
// checked as the import does when the library is mapped at the Bridge (else
// against the books listing only, its paths compared in any case). It asks
// Audible nothing, and reads each folder of the library at most once
// (dirCache), only folders a plan's path passes through that exist.
type titlePlanner struct {
	libraryID int64
	template  string // as the poll's params gave it
	planner   *importjob.Planner
	books     map[string]string // bookPaths; each path folded for an unmapped library
	tops      map[string]bool   // the listed books' top folders, folded
	fsys      transfer.LibraryFS
	root      *os.Root
}

// newTitlePlanner plans over books in libraryID; mount is "" when the library
// isn't mapped at the Bridge (no disk checks).
func newTitlePlanner(libraryID int64, template string, books []link.LibraryBook, mount string) *titlePlanner {
	t := &titlePlanner{libraryID: libraryID, template: template, planner: importjob.NewPlanner(books),
		tops: map[string]bool{}}
	for _, b := range books {
		if top, _, ok := strings.Cut(b.Path, "/"); ok {
			t.tops[strings.ToLower(top)] = true
		}
	}
	if mount != "" {
		if root, err := os.OpenRoot(mount); err == nil {
			t.root, t.fsys = root, newDirCache(transfer.RootFS(root))
		}
	}
	if t.fsys != nil {
		t.books = bookPaths(books)
		return t
	}
	// Without the disk, a listed book under another case is the same folder on
	// a case-insensitive disk (an SMB share, macOS, Windows; bridge#47): the
	// plan warns rather than says a new folder.
	t.books = make(map[string]string, len(books))
	for _, b := range books {
		k := strings.ToLower(b.Path)
		if _, ok := t.books[k]; !ok {
			t.books[k] = b.Title
		}
	}
	return t
}

// close lets the library's mount go.
func (t *titlePlanner) close() {
	if t.root != nil {
		_ = t.root.Close()
	}
}

// plan is rec's planned destination.
func (t *titlePlanner) plan(rec source.Record) *link.AudiblePlan {
	item, err := destination(ImportParams{Template: t.template}, rec, t.planner)
	p := &link.AudiblePlan{LibraryID: t.libraryID, Kind: planKind(item.Kind), Siblings: item.Siblings, Template: t.template}
	failed := func(err error) *link.AudiblePlan {
		p.Path, p.File, p.State, p.Conflict = "", "", link.PlanError, err.Error()
		return p
	}
	if err != nil {
		return failed(err)
	}
	rel := item.RelPath
	if t.fsys != nil {
		if rel, err = spelled(t.fsys, rel); err != nil {
			return failed(err)
		}
	}
	p.Path, p.File = rel, path.Base(rel)
	switch {
	case t.fsys != nil:
		conflict, err := vacant(t.fsys, rel, t.books)
		if err != nil && conflict == "" {
			return failed(err)
		}
		p.Conflict = conflict
	default:
		if _, _, ok := listedBook(strings.ToLower(rel), t.books); ok {
			p.Conflict = link.ConflictBook
		}
	}
	switch {
	case p.Conflict != "":
		p.State = link.PlanExists
	case item.Siblings > 0 || t.topExists(rel):
		p.State = link.PlanJoins
	default:
		p.State = link.PlanNew
	}
	return p
}

// topExists reports whether rel's top folder (the author's, in Auto) is one
// the library has: a listed book's, or a folder at the mount.
func (t *titlePlanner) topExists(rel string) bool {
	top, _, ok := strings.Cut(rel, "/")
	if !ok {
		return false
	}
	if t.tops[strings.ToLower(top)] {
		return true
	}
	if t.fsys == nil {
		return false
	}
	st, err := t.fsys.Lstat(top)
	return err == nil && st.IsDir()
}

// planKind is a placement kind on the wire.
func planKind(k placement.Kind) string {
	switch k {
	case placement.SeriesExisting:
		return link.PlanSeriesExisting
	case placement.SeriesNew:
		return link.PlanSeriesNew
	default:
		return link.PlanStandalone
	}
}

// dirCache is a library's files as a poll's plans read them: each folder
// listed, and each path looked at, once per poll (many titles share an author
// folder, and the library's root is every plan's first folder).
type dirCache struct {
	transfer.LibraryFS
	dirs  map[string]dirList
	stats map[string]statResult
}

type dirList struct {
	entries []fs.DirEntry
	err     error
}

type statResult struct {
	info fs.FileInfo
	err  error
}

func newDirCache(fsys transfer.LibraryFS) *dirCache {
	return &dirCache{LibraryFS: fsys, dirs: map[string]dirList{}, stats: map[string]statResult{}}
}

// ReadDir lists name once.
func (c *dirCache) ReadDir(name string) ([]fs.DirEntry, error) {
	if d, ok := c.dirs[name]; ok {
		return d.entries, d.err
	}
	entries, err := c.LibraryFS.ReadDir(name)
	c.dirs[name] = dirList{entries, err}
	return entries, err
}

// Lstat looks at name once.
func (c *dirCache) Lstat(name string) (fs.FileInfo, error) {
	if s, ok := c.stats[name]; ok {
		return s.info, s.err
	}
	info, err := c.LibraryFS.Lstat(name)
	c.stats[name] = statResult{info, err}
	return info, err
}
