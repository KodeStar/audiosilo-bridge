package generate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
)

// AttachDir is the book's attachment folder the server reads (contract section 4).
const AttachDir = ".audiosilo"

// The attachment names (a single-file book's get "<stem>." in front).
const (
	charactersName  = "characters.json"
	recapsName      = "recaps.json"
	descriptionName = "description.json"
	chaptersName    = "chapters.json"
	provenanceName  = "bridge.json"
)

// maxAttachment is the largest attachment the server reads (4 MiB); a bigger one
// would be ignored there, so it fails here instead.
const maxAttachment = 4 << 20

// sidecars are a finished engine book's validated characters and recaps, as the
// pipeline left them (pipeline.FinalSidecarsPaths), the description the run's
// description step read back (pipeline.FinalDescriptionPath), when it has one, and
// the chapters the run found in the narration (Phase 1d), when it found them.
type sidecars struct {
	characters, recaps, description, chapters []byte
	nChars, nRecaps                           int
	// workID is the slug the files name (the resolved work, or the pipeline's
	// placeholder).
	workID string
}

// hash identifies the content, so the same files aren't written twice.
func (s sidecars) hash() string {
	h := sha256.New()
	for _, b := range [][]byte{s.characters, s.recaps, s.description, s.chapters} {
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sidecarsSig is the size and modification time of the engine's sidecar files
// ("" when neither is there): a book parked at contributing is looked at every
// poll, and unchanged files needn't be read and decoded again.
func sidecarsSig(workDir string) string {
	chars, recaps := pipeline.FinalSidecarsPaths(workDir)
	var b strings.Builder
	for _, p := range []string{chars, recaps} {
		if info, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%d:%d;", info.Size(), info.ModTime().UnixNano())
		} else {
			b.WriteString("-;")
		}
	}
	if sig := b.String(); sig != "-;-;" {
		return sig
	}
	return ""
}

// readAttachment reads one of the engine's files for the library: nil when it
// isn't there, an error over the server's size cap.
func readAttachment(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the engine's own work dir
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
		return nil, fmt.Errorf("%s is larger than %d bytes, which the server won't read", filepath.Base(path), maxAttachment)
	}
	return b, nil
}

// readDescription reads the engine's description.json at path (nil when there is
// none): it must decode into the community model, which it returns too.
func readDescription(path string) ([]byte, model.Description, error) {
	var d model.Description
	b, err := readAttachment(path)
	if err != nil || b == nil {
		return nil, d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, d, fmt.Errorf("the engine's description.json doesn't decode: %w", err)
	}
	return b, d, nil
}

// readSidecars reads the engine's characters.json and recaps.json: each must
// decode into the community model and fit the server's size cap. A missing file is
// skipped; neither present is an error. The description is never read here: the
// run ships only the one its own step read back (write).
func readSidecars(workDir string) (sidecars, error) {
	var s sidecars
	charsPath, recapsPath := pipeline.FinalSidecarsPaths(workDir)
	var err error
	if s.characters, err = readAttachment(charsPath); err != nil {
		return sidecars{}, err
	}
	if s.recaps, err = readAttachment(recapsPath); err != nil {
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
	// Description is how description.json was made (Phase 1c; the server reads
	// only the fields above). Absent when no description was written with these.
	Description *DescriptionProvenance `json:"description,omitempty"`
	// Chapters is how chapters.json was made (Phase 1d). Absent when there is none.
	Chapters *ChaptersProvenance `json:"chapters,omitempty"`
}

// ChaptersProvenance is bridge.json's chapters object: which job wrote
// chapters.json and when, from what (the narration), and how many chapters.
type ChaptersProvenance struct {
	JobID       int64  `json:"job_id"`
	GeneratedAt string `json:"generated_at"`
	From        string `json:"from"`
	Chapters    int    `json:"chapters"`
}

// DescriptionProvenance is bridge.json's description object: which job wrote
// description.json and when, from what (the opening chapters' fact notes), and
// what its no-verbatim check measured against.
type DescriptionProvenance struct {
	JobID          int64  `json:"job_id"`
	GeneratedAt    string `json:"generated_at"`
	From           string `json:"from"`
	Chapters       int    `json:"chapters"`
	CheckedAgainst string `json:"checked_against"`
}

// descriptionFrom is the only source a description is written from.
const descriptionFrom = "opening_notes"

// descriptionProvenance composes the description object for res, written by job.
func descriptionProvenance(jobID int64, res pipeline.DescribeResult) *DescriptionProvenance {
	return &DescriptionProvenance{JobID: jobID, GeneratedAt: res.GeneratedAt.UTC().Format(time.RFC3339), From: descriptionFrom,
		Chapters: res.Chapters, CheckedAgainst: res.CheckedAgainst}
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
		if rel, err := filepath.Rel(mount, epub); err == nil && fsutil.Within(mount, epub) {
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

// attachmentFiles are the sidecars and the provenance as the files written into
// the book's .audiosilo/ (contract section 4); attachmentNames names them.
func attachmentFiles(s sidecars, prov Provenance) ([]attachFile, error) {
	provJSON, err := indentJSON(prov)
	if err != nil {
		return nil, err
	}
	return []attachFile{{charactersName, s.characters}, {recapsName, s.recaps}, {descriptionName, s.description},
		{chaptersName, s.chapters}, {provenanceName, provJSON}}, nil
}

// indentJSON is v as bridge.json is written: indented, a newline at the end.
func indentJSON(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// attachFile is one file for the book's .audiosilo/ (nil data: not written).
type attachFile struct {
	name string
	data []byte
}

// writeAttachFiles writes files into dir/.audiosilo/. The folder is made 0755 if
// missing and refused when it is a link or not a folder; each file goes to a temp
// file inside it, 0644, then is renamed into place, so the server never reads half
// a file. Only the known JSON names are ever written there.
func writeAttachFiles(dir, prefix string, files []attachFile) error {
	adir, err := attachFolder(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.data == nil {
			continue
		}
		// 0644: the server (another user) reads it.
		if err := fsutil.WriteFileAtomic(filepath.Join(adir, prefix+f.name), f.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// attachFolder is dir/.audiosilo/, made 0755 when missing; a link or a file
// there is refused.
func attachFolder(dir string) (string, error) {
	adir := filepath.Join(dir, AttachDir)
	info, err := os.Lstat(adir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(adir, 0o755); err != nil { //nolint:gosec // the server (another user) reads it
			return "", err
		}
		// The umask may have narrowed it; the server must be able to list it.
		if err := os.Chmod(adir, 0o755); err != nil { //nolint:gosec // as above
			return "", err
		}
	case err != nil:
		return "", err
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return "", fmt.Errorf("the book's %s exists and isn't a folder; the Bridge won't write through it", AttachDir)
	}
	return adir, nil
}

// maxProvenance bounds the read of an existing bridge.json (the Bridge's own,
// a few hundred bytes).
const maxProvenance = 256 << 10

// describedProvenance is bridge.json after a description-only job: the existing
// file with its description object added or replaced (every other field kept as
// it is), or, when there is none (or it isn't a JSON object, or isn't a regular
// file), fresh with the usual fields.
func describedProvenance(dir, prefix string, fresh Provenance) ([]byte, error) {
	return mergedProvenance(dir, prefix, fresh, "description")
}

// mergedProvenance is the book's bridge.json with its key object set to fresh's
// (every other field kept as it is), or fresh when there is none fit to keep: a
// job that writes one attachment (a description, the chapters) vouches for it
// without unsaying what made the others.
func mergedProvenance(dir, prefix string, fresh Provenance, key string) ([]byte, error) {
	existing := readProvenance(filepath.Join(dir, AttachDir), prefix+provenanceName)
	if existing == nil {
		return indentJSON(fresh)
	}
	all, err := json.Marshal(fresh)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(all, &fields); err != nil {
		return nil, err
	}
	existing[key] = json.RawMessage("null") // an object fresh omits is none
	if v, ok := fields[key]; ok {
		existing[key] = v
	}
	return indentJSON(existing)
}

// keptObject is the key object (T) of the book's bridge.json, when the attachment
// file it vouches for is in place beside it: a run that writes no such file of its
// own (a description, the chapters) leaves an earlier run's there (nothing deletes
// into a library), and the new bridge.json carries over what vouches for it. nil
// when either is missing, isn't a regular file, or holds no such object.
func keptObject[T any](dir, prefix, file, key string) *T {
	adir := filepath.Join(dir, AttachDir)
	if info, err := os.Lstat(filepath.Join(adir, prefix+file)); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, ok := readProvenance(adir, prefix+provenanceName)[key]
	if !ok {
		return nil
	}
	var v *T
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// readProvenance reads adir/name (a bridge.json) as a JSON object, nil when there
// is none fit to keep. The library is another program's folder, so the read is
// the agent container's kind (agentproto.ReadRegular, which generate may not
// import): inside adir as an os.Root, of the regular file that was looked at (a
// link, FIFO or device swapped in after the look is refused, the open never
// blocking), capped. A .audiosilo that is itself a link is not read through.
func readProvenance(adir, name string) map[string]json.RawMessage {
	if info, err := os.Lstat(adir); err != nil || !info.IsDir() {
		return nil
	}
	root, err := os.OpenRoot(adir)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxProvenance {
		return nil
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !os.SameFile(before, info) {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxProvenance+1))
	if err != nil || len(raw) > maxProvenance {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	return m
}

// provenance composes bridge.json for a job of kind.
func provenance(version, kind string, jobID int64, now time.Time, source, workID string, inputs []InputFile) Provenance {
	return Provenance{Generator: "audiosilo-bridge", Version: version, Kind: kind, JobID: jobID,
		GeneratedAt: now.UTC().Format(time.RFC3339), Source: source, WorkID: workID, Inputs: inputs}
}
