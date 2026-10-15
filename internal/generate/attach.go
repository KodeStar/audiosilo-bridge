package generate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// AttachDir is the book's attachment folder the server reads (contract section 4).
const AttachDir = ".audiosilo"

// The attachment names (a single-file book's get "<stem>." in front).
const (
	charactersName = "characters.json"
	recapsName     = "recaps.json"
	provenanceName = "bridge.json"
)

// maxAttachment is the largest attachment the server reads (4 MiB); a bigger one
// would be ignored there, so it fails here instead.
const maxAttachment = 4 << 20

// sidecars are a finished engine book's validated characters and recaps, as the
// pipeline left them in <workDir>/sidecars/.
type sidecars struct {
	characters, recaps []byte
	nChars, nRecaps    int
	// workID is the slug the files name (the resolved work, or the pipeline's
	// placeholder).
	workID string
}

// hash identifies the content, so the same files aren't written twice.
func (s sidecars) hash() string {
	h := sha256.New()
	_, _ = h.Write(s.characters)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(s.recaps)
	return hex.EncodeToString(h.Sum(nil))
}

// readSidecars reads <workDir>/sidecars/{characters,recaps}.json: each must decode
// into the community model and fit the server's size cap. A missing file is
// skipped; neither present is an error.
func readSidecars(workDir string) (sidecars, error) {
	var s sidecars
	dir := filepath.Join(workDir, "sidecars")
	read := func(name string) ([]byte, error) {
		f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // the engine's own work dir
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxAttachment+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxAttachment {
			return nil, fmt.Errorf("%s is larger than %d bytes, which the server won't read", name, maxAttachment)
		}
		return b, nil
	}
	var err error
	if s.characters, err = read(charactersName); err != nil {
		return sidecars{}, err
	}
	if s.recaps, err = read(recapsName); err != nil {
		return sidecars{}, err
	}
	if s.characters == nil && s.recaps == nil {
		return sidecars{}, errors.New("the engine left no characters or recaps")
	}
	if s.characters != nil {
		var c model.Characters
		if err := json.Unmarshal(s.characters, &c); err != nil {
			return sidecars{}, fmt.Errorf("the engine's characters.json doesn't decode: %w", err)
		}
		s.nChars, s.workID = len(c.Characters), c.Work
	}
	if s.recaps != nil {
		var r model.Recaps
		if err := json.Unmarshal(s.recaps, &r); err != nil {
			return sidecars{}, fmt.Errorf("the engine's recaps.json doesn't decode: %w", err)
		}
		s.nRecaps = len(r.Recaps)
		if s.workID == "" {
			s.workID = r.Work
		}
	}
	return s, nil
}

// Provenance is bridge.json: what made the attachments, from what.
type Provenance struct {
	Generator   string      `json:"generator"`
	Version     string      `json:"version"`
	Kind        string      `json:"kind"`
	JobID       int64       `json:"job_id"`
	GeneratedAt string      `json:"generated_at"`
	Source      string      `json:"source"`
	WorkID      string      `json:"work_id"`
	Inputs      []InputFile `json:"inputs"`
}

// InputFile is one file the run read, its path relative to the library root.
type InputFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

// inputsOf lists what the run read: the book's audio files as the server sees
// them, or the epub (stat-ed here, its path made relative to the mount).
func inputsOf(mount string, files []link.BookFile, epub string) []InputFile {
	if epub != "" {
		in := InputFile{Path: filepath.Base(epub)}
		if rel, err := filepath.Rel(mount, epub); err == nil && !strings.HasPrefix(rel, "..") {
			in.Path = filepath.ToSlash(rel)
		}
		if info, err := os.Stat(epub); err == nil {
			in.Size, in.ModTime = info.Size(), info.ModTime().Unix()
		}
		return []InputFile{in}
	}
	out := make([]InputFile, 0, len(files))
	for _, f := range files {
		out = append(out, InputFile{Path: f.Path, Size: f.Size, ModTime: f.ModTime})
	}
	return out
}

// writeAttachments writes the sidecars and the provenance into dir/.audiosilo/
// (contract section 4) and returns the names written. The folder is made 0755 if
// missing and refused when it is a link or not a folder; each file goes to a
// temp file inside it, 0644, then is renamed into place, so the server never
// reads half a file. Only these JSON names are ever written there.
func writeAttachments(dir, prefix string, s sidecars, prov Provenance) ([]string, error) {
	adir := filepath.Join(dir, AttachDir)
	info, err := os.Lstat(adir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(adir, 0o755); err != nil { //nolint:gosec // the server (another user) reads it
			return nil, err
		}
		// The umask may have narrowed it; the server must be able to list it.
		if err := os.Chmod(adir, 0o755); err != nil { //nolint:gosec // as above
			return nil, err
		}
	case err != nil:
		return nil, err
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return nil, fmt.Errorf("%s exists and isn't a folder; the Bridge won't write through it", adir)
	}
	provJSON, err := json.MarshalIndent(prov, "", "  ")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range []struct {
		name string
		data []byte
	}{{charactersName, s.characters}, {recapsName, s.recaps}, {provenanceName, append(provJSON, '\n')}} {
		if f.data == nil {
			continue
		}
		if err := writeFile(adir, prefix+f.name, f.data); err != nil {
			return names, err
		}
		names = append(names, prefix+f.name)
	}
	return names, nil
}

// writeFile writes data to adir/name through a temp file in adir, renamed into
// place (0644).
func writeFile(adir, name string, data []byte) error {
	tmp, err := os.CreateTemp(adir, ".bridge-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { //nolint:gosec // the server (another user) reads it
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(adir, name)); err != nil {
		return err
	}
	done = true
	return nil
}

// provenance composes bridge.json.
func provenance(version string, jobID int64, now time.Time, source, workID string, inputs []InputFile) Provenance {
	return Provenance{Generator: "audiosilo-bridge", Version: version, Kind: Kind, JobID: jobID,
		GeneratedAt: now.UTC().Format(time.RFC3339), Source: source, WorkID: workID, Inputs: inputs}
}
