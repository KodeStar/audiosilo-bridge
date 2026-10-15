package generate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/extract"

	"github.com/kodestar/audiosilo-bridge/internal/ebook"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
)

// place is where a book is on the Bridge's side of a checked mapping.
type place struct {
	// full is the book's own path through the mount: its folder, or for a
	// single-file book the file.
	full string
	// dir is the folder that holds the book's .audiosilo/: the book's folder, or
	// the folder holding a single file.
	dir string
	// prefix starts the attachment names: "" for a folder book, "<stem>." for a
	// single-file book (stem = the file name without its last extension).
	prefix string
	single bool
}

// locate resolves a book's library-relative path through the mount (SafeJoin:
// never outside it).
func locate(mount, rel string) (place, error) {
	full, err := mapping.SafeJoin(mount, rel)
	if err != nil {
		return place{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return place{}, fmt.Errorf("the book isn't there through the mapping: %w", err)
	}
	if info.IsDir() {
		return place{full: full, dir: full}, nil
	}
	name := filepath.Base(full)
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	return place{full: full, dir: filepath.Dir(full), prefix: stem + ".", single: true}, nil
}

// epubBeside finds the book's own .epub: for a folder book exactly one directly
// in its folder (never a subfolder, never a dot file such as macOS's "._x.epub"
// stub), for a single-file book "<stem>.epub" beside the file (its folder holds
// other books). "" when there is none; an error when a folder holds several.
func epubBeside(p place) (string, error) {
	if p.single {
		cand := filepath.Join(p.dir, strings.TrimSuffix(p.prefix, ".")+".epub")
		if info, err := os.Stat(cand); err == nil && info.Mode().IsRegular() {
			return cand, nil
		}
		return "", nil
	}
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !ebook.IsEpub(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		found = append(found, filepath.Join(p.dir, e.Name()))
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("the book's folder holds %d .epub files; keep one", len(found))
}

// readEpub reports whether an epub can be read (its OPF parses; a DRM-wrapped or
// corrupt file can't), so the engine never gets one it can't extract.
var readEpub = func(path string) error {
	_, err := extract.ReadMetadata(path)
	return err
}

// errNoEbook is a source=ebook job for a book with no readable .epub.
var errNoEbook = errors.New("there is no readable .epub in the book's own folder")

// chooseSource picks what the run reads: the source the params ask for, and the
// epub when it is an ebook run. note says why auto fell back to the audio ("" when
// it didn't).
func chooseSource(p place, want string) (source, epub, note string, err error) {
	if want == SourceAudio {
		return SourceAudio, "", "", nil
	}
	cand, ferr := epubBeside(p)
	if ferr == nil && cand != "" {
		ferr = readEpub(cand)
		if ferr != nil {
			ferr = fmt.Errorf("%s can't be read (it may be DRM-protected): %w", filepath.Base(cand), ferr)
		}
	}
	switch {
	case ferr == nil && cand != "":
		return SourceEbook, cand, "", nil
	case want == SourceEbook && ferr != nil:
		return "", "", "", ferr
	case want == SourceEbook:
		return "", "", "", errNoEbook
	case ferr != nil:
		return SourceAudio, "", ferr.Error() + "; reading the audio instead", nil
	}
	return SourceAudio, "", "", nil
}
