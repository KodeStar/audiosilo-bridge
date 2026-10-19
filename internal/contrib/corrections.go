package contrib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-meta/pkg/model"
)

// The correction-batch form (audiosilo-meta's
// .github/ISSUE_TEMPLATE/correction-batch.yml, read by internal/issueform's
// correctBatch). Its headings, checkbox text, labels, bounds and field table are
// copied from the form and compose_batch.go VERBATIM; meta's TestBatchGoldenBody
// pins the body this file renders (TestCorrectionBatchGoldenBody holds the twin).
const (
	hRecord             = "Record"
	hChanges            = "Changes"
	hEvidence           = "Evidence / source"
	itemCC0Corrections  = "I dedicate these corrections to the public domain under CC0-1.0, and I have the right to do so."
	correctionsTitleTag = "[corrections] "

	// MaxCorrections is the most changes one issue carries (meta's maxBatchChanges).
	MaxCorrections = 25
	// MaxCorrectionValue bounds each value, in characters (maxBatchValueRunes).
	MaxCorrectionValue = 500
	// MaxCorrectionEvidence bounds the evidence, in characters (maxBatchEvidenceRunes).
	MaxCorrectionEvidence = 2000
	// maxSource bounds the stamped source ("audiosilo-server/<version>").
	maxSource = 100
	// batchVersion is the Changes document's version.
	batchVersion = 1
)

var correctionLabels = []string{"data", "data:correction-batch"}

// The server's correctable fields (CORRECTIONS-SPIKE.md section 5, plan D5): the
// names the console's book page edits and the meta.correct job carries.
const (
	CorrectTitle       = "title"
	CorrectAuthor      = "author"
	CorrectNarrator    = "narrator"
	CorrectSeries      = "series"
	CorrectSeriesIndex = "series_index"
	CorrectMoreSeries  = "more_series"
	CorrectPublished   = "published"
	CorrectISBN        = "isbn"
)

// correctionTarget is where a server field lands in meta: the record kind, meta's
// field name, and whether meta's bot applies it (auto) or a maintainer does.
type correctionTarget struct {
	recording bool
	field     string
	auto      bool
}

// correctionFields maps the server's fields onto meta's batch vocabulary
// (compose_batch.go batchFields). Any other field is refused.
var correctionFields = map[string]correctionTarget{
	CorrectTitle:       {field: "title", auto: true},
	CorrectAuthor:      {field: "authors"},
	CorrectSeries:      {field: "series"},
	CorrectSeriesIndex: {field: "series_index"},
	CorrectMoreSeries:  {field: "more_series"},
	CorrectPublished:   {recording: true, field: "release_date", auto: true},
	CorrectISBN:        {recording: true, field: "isbn", auto: true},
	CorrectNarrator:    {recording: true, field: "narrators"},
}

// ErrInvalidCorrection matches every refusal of a correction batch (errors.Is);
// the refusal's text is the reason alone.
var ErrInvalidCorrection = errors.New("invalid correction")

type invalidCorrection struct{ reason string }

func (e invalidCorrection) Error() string        { return e.reason }
func (e invalidCorrection) Is(target error) bool { return target == ErrInvalidCorrection }

// Correction is one server field's edit: what the community record says and what
// the admin set.
type Correction struct {
	Field     string `json:"field"`
	Current   string `json:"current"`
	Corrected string `json:"corrected"`
}

// CorrectionBatch is one book's corrections for one community work.
type CorrectionBatch struct {
	WorkID string
	// WorkTitle names the issue; the work id when empty.
	WorkTitle string
	// RecordingID addresses the recording fields; without it they are addressed to
	// the work, which meta hands to a maintainer (who finds the recording).
	RecordingID string
	Changes     []Correction
	Evidence    string
	// Source is the stamp the server put on the job ("audiosilo-server/<version>").
	Source string
}

// MappedChange is one change as filed.
type MappedChange struct {
	Record    string `json:"record"`
	Field     string `json:"field"`
	Current   string `json:"current"`
	Corrected string `json:"corrected"`
	// ServerField is the server's name for it; Auto whether meta's bot applies it.
	ServerField string `json:"-"`
	Auto        bool   `json:"-"`
}

// workRecord is a work's record reference (meta's workPageURL).
func workRecord(workID string) string { return model.SiteURL + "/works/" + workID }

// recordingRecord is a recording's record reference (meta's recordingRef).
func recordingRecord(workID, recID string) string {
	return "data/works/" + workID + "/recordings/" + recID
}

// MapCorrections checks a batch against meta's bounds and maps each change onto
// its record, refusing the whole batch for any bad change (as meta does).
func MapCorrections(b CorrectionBatch) ([]MappedChange, error) {
	bad := func(format string, args ...any) error {
		return invalidCorrection{fmt.Sprintf(format, args...)}
	}
	if !model.ValidSlug(b.WorkID) {
		return nil, bad("work id %q is not a meta slug", b.WorkID)
	}
	if b.RecordingID != "" && !model.ValidSlug(b.RecordingID) {
		return nil, bad("recording id %q is not a meta slug", b.RecordingID)
	}
	if n := len(b.Changes); n == 0 || n > MaxCorrections {
		return nil, bad("a correction holds 1 to %d changes, not %d", MaxCorrections, n)
	}
	ev := strings.TrimSpace(b.Evidence)
	if ev == "" {
		return nil, bad("the evidence is required")
	}
	if n := utf8.RuneCountInString(ev); n > MaxCorrectionEvidence {
		return nil, bad("the evidence is %d characters long; the limit is %d", n, MaxCorrectionEvidence)
	}
	// Meta bounds the evidence as the issue carries it, its # lines escaped.
	if n := utf8.RuneCountInString(escapeHeadings(ev)); n > MaxCorrectionEvidence {
		return nil, bad("the evidence is %d characters long once its lines starting with # are escaped; the limit is %d",
			n, MaxCorrectionEvidence)
	}
	if s := strings.TrimSpace(b.Source); s == "" || len(s) > maxSource || hasControl(s) {
		return nil, bad("the source stamp is missing or malformed")
	}
	out := make([]MappedChange, 0, len(b.Changes))
	seen := map[string]bool{}
	for i, c := range b.Changes {
		t, ok := correctionFields[c.Field]
		if !ok {
			return nil, bad("change %d: field %q is not one a correction takes", i+1, c.Field)
		}
		cur, cor := strings.TrimSpace(c.Current), strings.TrimSpace(c.Corrected)
		if cor == "" {
			return nil, bad("change %d: %s has no corrected value", i+1, c.Field)
		}
		for _, v := range []string{cur, cor} {
			if n := utf8.RuneCountInString(v); n > MaxCorrectionValue {
				return nil, bad("change %d: a value is %d characters long; the limit is %d", i+1, n, MaxCorrectionValue)
			}
			if hasControl(v) {
				return nil, bad("change %d: a value contains a control character", i+1)
			}
		}
		m := MappedChange{Record: workRecord(b.WorkID), Field: t.field, Current: cur, Corrected: cor, ServerField: c.Field, Auto: t.auto}
		if t.recording {
			if b.RecordingID != "" {
				m.Record = recordingRecord(b.WorkID, b.RecordingID)
			} else {
				m.Auto = false // meta hands a recording field on a work to a maintainer
			}
		}
		key := m.Record + "\x00" + m.Field
		if m.Field == "isbn" {
			key += "\x00" + m.Corrected // two ISBNs are two facts
		}
		if seen[key] {
			return nil, bad("change %d: %s is stated twice", i+1, c.Field)
		}
		seen[key] = true
		out = append(out, m)
	}
	return out, nil
}

// CorrectionBatchIssue renders a correction batch as meta's correction-batch
// issue form, returning the mapped changes beside it.
func CorrectionBatchIssue(b CorrectionBatch) (title, body string, labels []string, changes []MappedChange, err error) {
	changes, err = MapCorrections(b)
	if err != nil {
		return "", "", nil, nil, err
	}
	var doc bytes.Buffer
	enc := json.NewEncoder(&doc)
	enc.SetEscapeHTML(false) // readable in the issue; the same JSON either way
	if err := enc.Encode(struct {
		V       int            `json:"v"`
		Source  string         `json:"source"`
		Changes []MappedChange `json:"changes"`
	}{batchVersion, strings.TrimSpace(b.Source), changes}); err != nil {
		return "", "", nil, nil, err
	}
	name := strings.Join(strings.Fields(b.WorkTitle), " ")
	if name == "" {
		name = b.WorkID
	}
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:200])
	}
	var sb strings.Builder
	sb.WriteString(field(hRecord, workRecord(b.WorkID)))
	sb.WriteString(attachmentSection(hChanges, "", doc.Bytes(), ""))
	sb.WriteString(field(hEvidence, escapeHeadings(strings.TrimSpace(b.Evidence))))
	// The upper-case X is meta's golden body's; its parser takes either case.
	sb.WriteString("### " + hPublicDedication + "\n\n- [X] " + itemCC0Corrections + "\n")
	return correctionsTitleTag + name, sb.String(), cloneLabels(correctionLabels), changes, nil
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// escapeHeadings escapes a line of free text that would start a form section
// ("### ..."), so evidence can't add or shadow one of the form's fields.
func escapeHeadings(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimLeft(l, " \t"), "#") {
			lines[i] = "\\" + strings.TrimLeft(l, " \t")
		}
	}
	return strings.Join(lines, "\n")
}
