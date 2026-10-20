package contrib

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func goldenBatch() CorrectionBatch {
	return CorrectionBatch{
		WorkID: "existing-work", WorkTitle: "Existing Work", RecordingID: "john-smith-2020",
		Source: "audiosilo-server/2.4.0",
		Changes: []Correction{
			{Field: "title", Current: "Existing Work", Corrected: "The Existing Work"},
			{Field: "published", Current: "", Corrected: "2020-03-04"},
			{Field: "isbn", Current: "", Corrected: "9781473647633"},
			{Field: "author", Current: "Jane Doe", Corrected: "Jane Doe, John Smith"},
		},
		Evidence: "Edited by the server admin; the publisher's page lists the narrator as co-author.",
	}
}

// TestCorrectionBatchGoldenBody is the twin of audiosilo-meta's TestBatchGoldenBody
// (internal/issueform/compose_batch_test.go): the same body, except that every
// change names its record (meta's golden leaves the last one to default to the
// Record field, which is the same work).
func TestCorrectionBatchGoldenBody(t *testing.T) {
	const want = "### Record\n\nhttps://meta.audiosilo.app/works/existing-work\n\n" +
		"### Changes\n\n```json\n" +
		`{"v":1,"source":"audiosilo-server/2.4.0","changes":[` +
		`{"record":"https://meta.audiosilo.app/works/existing-work","field":"title","current":"Existing Work","corrected":"The Existing Work"},` +
		`{"record":"data/works/existing-work/recordings/john-smith-2020","field":"release_date","current":"","corrected":"2020-03-04"},` +
		`{"record":"data/works/existing-work/recordings/john-smith-2020","field":"isbn","current":"","corrected":"9781473647633"},` +
		`{"record":"https://meta.audiosilo.app/works/existing-work","field":"authors","current":"Jane Doe","corrected":"Jane Doe, John Smith"}]}` +
		"\n```\n\n" +
		"### Evidence / source\n\nEdited by the server admin; the publisher's page lists the narrator as co-author.\n\n" +
		"### Public domain dedication\n\n- [X] I dedicate these corrections to the public domain under CC0-1.0, and I have the right to do so.\n"
	title, body, labels, changes, err := CorrectionBatchIssue(goldenBatch())
	if err != nil {
		t.Fatal(err)
	}
	if body != want {
		t.Fatalf("body =\n%s\nwant\n%s", body, want)
	}
	if title != "[corrections] Existing Work" {
		t.Fatalf("title = %q", title)
	}
	if !slices.Equal(labels, []string{"data", "data:correction-batch"}) {
		t.Fatalf("labels = %v", labels)
	}
	var auto, maint []string
	for _, c := range changes {
		if c.Auto {
			auto = append(auto, c.ServerField)
		} else {
			maint = append(maint, c.ServerField)
		}
	}
	if strings.Join(auto, ",") != "title,published,isbn" || strings.Join(maint, ",") != "author" {
		t.Fatalf("auto = %v, maintainer = %v", auto, maint)
	}
	labels[0] = "mutated"
	if _, _, l, _, _ := CorrectionBatchIssue(goldenBatch()); l[0] != "data" {
		t.Fatal("labels are shared")
	}
}

// TestCorrectionFieldMapping pins every server field onto meta's batchFields.
func TestCorrectionFieldMapping(t *testing.T) {
	work, rec := "https://meta.audiosilo.app/works/w", "data/works/w/recordings/r"
	cases := []struct {
		field, record, meta string
		auto                bool
	}{
		{"title", work, "title", true},
		{"author", work, "authors", false},
		{"series", work, "series", false},
		{"series_index", work, "series_index", false},
		{"more_series", work, "more_series", false},
		{"published", rec, "release_date", true},
		{"isbn", rec, "isbn", true},
		{"narrator", rec, "narrators", false},
	}
	for _, c := range cases {
		got, err := MapCorrections(CorrectionBatch{WorkID: "w", RecordingID: "r", Source: "s/1", Evidence: "e",
			Changes: []Correction{{Field: c.field, Corrected: "1"}}})
		if err != nil {
			t.Fatalf("%s: %v", c.field, err)
		}
		if g := got[0]; g.Record != c.record || g.Field != c.meta || g.Auto != c.auto || g.ServerField != c.field {
			t.Errorf("%s -> %+v", c.field, g)
		}
	}
	// Without a recording id a recording field addresses the work, for a maintainer.
	got, err := MapCorrections(CorrectionBatch{WorkID: "w", Source: "s/1", Evidence: "e",
		Changes: []Correction{{Field: "isbn", Corrected: "9781473647633"}, {Field: "narrator", Corrected: "A"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.Record != work || g.Auto {
			t.Errorf("no recording: %+v", g)
		}
	}
}

func TestCorrectionBatchRefusals(t *testing.T) {
	long := strings.Repeat("a", MaxCorrectionValue+1)
	many := make([]Correction, MaxCorrections+1)
	for i := range many {
		many[i] = Correction{Field: "isbn", Corrected: strings.Repeat("9", i+1)}
	}
	cases := map[string]func(*CorrectionBatch){
		"unknown field":   func(b *CorrectionBatch) { b.Changes[0].Field = "subtitle" },
		"meta field name": func(b *CorrectionBatch) { b.Changes[0].Field = "authors" },
		"empty corrected": func(b *CorrectionBatch) { b.Changes[0].Corrected = "  " },
		"long value":      func(b *CorrectionBatch) { b.Changes[0].Corrected = long },
		"long current":    func(b *CorrectionBatch) { b.Changes[0].Current = long },
		"control char":    func(b *CorrectionBatch) { b.Changes[0].Corrected = "a\nb" },
		"no changes":      func(b *CorrectionBatch) { b.Changes = nil },
		"too many":        func(b *CorrectionBatch) { b.Changes = many },
		"no evidence":     func(b *CorrectionBatch) { b.Evidence = " " },
		"long evidence":   func(b *CorrectionBatch) { b.Evidence = strings.Repeat("e", MaxCorrectionEvidence+1) },
		// Within the bound as written, over it as filed (each # line gains a backslash).
		"long escaped evidence": func(b *CorrectionBatch) {
			b.Evidence = strings.Repeat("#a\n", 600) + strings.Repeat("e", MaxCorrectionEvidence-1800)
		},
		"no source":         func(b *CorrectionBatch) { b.Source = "" },
		"bad work":          func(b *CorrectionBatch) { b.WorkID = "Not A Slug" },
		"bad recording":     func(b *CorrectionBatch) { b.RecordingID = "../x" },
		"stated twice":      func(b *CorrectionBatch) { b.Changes[1] = b.Changes[0] },
		"isbn stated twice": func(b *CorrectionBatch) { b.Changes[1] = b.Changes[2] },
	}
	for name, mut := range cases {
		b := goldenBatch()
		mut(&b)
		if _, _, _, _, err := CorrectionBatchIssue(b); !errors.Is(err, ErrInvalidCorrection) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Two different ISBNs are two facts; the evidence can't open a form section;
	// the title falls back to the work id.
	b := goldenBatch()
	b.Changes = append(b.Changes, Correction{Field: "isbn", Corrected: "9780000000002"})
	b.Evidence = "line one\n### Changes\nmore"
	b.WorkTitle = ""
	title, body, _, _, err := CorrectionBatchIssue(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(body, "\n### Changes") != 1 || !strings.Contains(body, "\\### Changes") {
		t.Fatalf("evidence heading not escaped:\n%s", body)
	}
	if title != "[corrections] existing-work" {
		t.Fatalf("title = %q", title)
	}
}
