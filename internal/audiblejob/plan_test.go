package audiblejob

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// TestPlanIsTheImportsDestination: a title's planned path is the import's
// Destination for the same record, books and template (Auto and templates),
// and a destination the import refuses is a plan in state error saying why,
// mapped or not.
func TestPlanIsTheImportsDestination(t *testing.T) {
	t.Parallel()
	books := goldenBooks()
	for _, mount := range []string{"", t.TempDir()} {
		for _, c := range goldenCases() {
			if c.p.DestPath != "" {
				continue // a dest_path is checked when the backup starts, never planned
			}
			tp := newTitlePlanner(1, c.p.Template, books, mount)
			got := tp.plan(c.rec)
			tp.close()
			want, err := Destination(c.p, c.rec, books)
			switch {
			case err != nil:
				if got.State != link.PlanError || got.Path != "" || got.File != "" || got.Conflict != err.Error() {
					t.Errorf("%s (mount %q): %+v, want error %q", c.name, mount, got, err)
				}
			case got.Path != want || got.File != filepath.Base(want) || got.State == link.PlanError || got.State == link.PlanExists:
				t.Errorf("%s (mount %q): %+v, want %q", c.name, mount, got, want)
			}
			if got.LibraryID != 1 || got.Template != c.p.Template {
				t.Errorf("%s: library %d template %q", c.name, got.LibraryID, got.Template)
			}
		}
	}
}

// TestPlanStates: new, joins (beside the series' books, or in an author folder
// the library has), exists (a file at the path, a listed book's folder, a
// folder already holding audio), and error (a template that can't place it, a
// path the Bridge can't follow); without a mapping only the books listing is
// consulted.
func TestPlanStates(t *testing.T) {
	t.Parallel()
	books := goldenBooks()
	mount := t.TempDir()
	mk := func(rel string) {
		t.Helper()
		p := filepath.Join(mount, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("Ada Fenwick/Taken/Taken.m4b")            // a file at the planned path
	mk("Ada Fenwick/Loud/old.mp3")               // audio in the planned folder
	mk("Ada Fenwick/Quiet/cover.jpg")            // no audio: still vacant
	mk("Blocked")                                // a file where a folder goes
	mk("Disk Only/Something/Something Else.m4b") // an author folder only on disk
	// Backed up before bridge#47, under the names the rule gave then.
	mk("Spencer Johnson/Who Moved My Cheese?/Who Moved My Cheese?.m4b")
	mk(`Ada "Lace" Fenwick/Why  Not/Why  Not.m4b`)
	mk("Spencer Johnson/Out of the Maze?/cover.jpg") // no audio: still vacant

	// named is rec with its file named as the Audible client names it now.
	named := func(file string, r source.Record) source.Record {
		r.FileName = file
		return r
	}
	cases := []struct {
		name     string
		template string
		rec      source.Record
		mapped   bool
		path     string
		kind     string
		siblings int
		state    string
		conflict string
	}{
		{"series beside its books", "", rec("B0PLAN0002", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2), true,
			"Mara Quill/Orchard Cycle/OC02 - The Copper Grove/The Copper Grove.m4b", link.PlanSeriesExisting, 2, link.PlanJoins, ""},
		{"a new author", "", rec("B0PLAN0003", "Lone Star", "New Writer", "", 0), true,
			"New Writer/Lone Star/Lone Star.m4b", link.PlanStandalone, 0, link.PlanNew, ""},
		{"a new series of a listed author", "", rec("B0PLAN0004", "First Light", "Theo Marchetti", "Dawn Road", 1), true,
			"Theo Marchetti/Dawn Road/DR01 - First Light/First Light.m4b", link.PlanSeriesNew, 0, link.PlanJoins, ""},
		{"an author folder on disk only", "", rec("B0PLAN0005", "Other", "Disk Only", "", 0), true,
			"Disk Only/Other/Other.m4b", link.PlanStandalone, 0, link.PlanJoins, ""},
		{"an author folder on disk, unmapped", "", rec("B0PLAN0005", "Other", "Disk Only", "", 0), false,
			"Disk Only/Other/Other.m4b", link.PlanStandalone, 0, link.PlanNew, ""},
		{"a file at the path", "", rec("B0PLAN0006", "Taken", "Ada Fenwick", "", 0), true,
			"Ada Fenwick/Taken/Taken.m4b", link.PlanStandalone, 0, link.PlanExists, link.ConflictFile},
		{"a file at the path, unmapped", "", rec("B0PLAN0006", "Taken", "Ada Fenwick", "", 0), false,
			"Ada Fenwick/Taken/Taken.m4b", link.PlanStandalone, 0, link.PlanNew, ""},
		{"audio in the folder", "", rec("B0PLAN0007", "Loud", "Ada Fenwick", "", 0), true,
			"Ada Fenwick/Loud/Loud.m4b", link.PlanStandalone, 0, link.PlanExists, link.ConflictAudio},
		{"no audio in the folder", "", rec("B0PLAN0008", "Quiet", "Ada Fenwick", "", 0), true,
			"Ada Fenwick/Quiet/Quiet.m4b", link.PlanStandalone, 0, link.PlanJoins, ""},
		{"a listed book's folder", "Theo Marchetti/A Small Hour in Varrow", rec("B0PLAN0009", "Varrow Again", "Theo Marchetti", "", 0), true,
			"Theo Marchetti/A Small Hour in Varrow/Varrow Again.m4b", link.PlanStandalone, 0, link.PlanExists, link.ConflictBook},
		{"a listed book's folder, unmapped", "Theo Marchetti/A Small Hour in Varrow", rec("B0PLAN0009", "Varrow Again", "Theo Marchetti", "", 0), false,
			"Theo Marchetti/A Small Hour in Varrow/Varrow Again.m4b", link.PlanStandalone, 0, link.PlanExists, link.ConflictBook},
		{"backed up under the former name", "", named("Who Moved My Cheese.m4b", rec("B0PLAN0012", "Who Moved My Cheese?", "Spencer Johnson", "", 0)), true,
			"Spencer Johnson/Who Moved My Cheese/Who Moved My Cheese.m4b", link.PlanStandalone, 0, link.PlanExists, link.ConflictAudio},
		{"backed up under the former name, unmapped", "", named("Who Moved My Cheese.m4b", rec("B0PLAN0012", "Who Moved My Cheese?", "Spencer Johnson", "", 0)), false,
			"Spencer Johnson/Who Moved My Cheese/Who Moved My Cheese.m4b", link.PlanStandalone, 0, link.PlanNew, ""},
		{"an author and a title under former names", "", named("Why Not.m4b", rec("B0PLAN0013", "Why  Not", `Ada "Lace" Fenwick`, "", 0)), true,
			"Ada 'Lace' Fenwick/Why Not/Why Not.m4b", link.PlanStandalone, 0, link.PlanExists, link.ConflictAudio},
		{"a former name's folder without audio", "", named("Out of the Maze.m4b", rec("B0PLAN0014", "Out of the Maze?", "Spencer Johnson", "", 0)), true,
			"Spencer Johnson/Out of the Maze/Out of the Maze.m4b", link.PlanStandalone, 0, link.PlanJoins, ""},
		{"a bad template", "{Author}/{Nope}", rec("B0PLAN0010", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2), true,
			"", link.PlanSeriesExisting, 2, link.PlanError, "*"},
		{"a file where a folder goes", "Blocked/{Title}", rec("B0PLAN0011", "Inside", "Ada Fenwick", "", 0), true,
			"", link.PlanStandalone, 0, link.PlanError, "*"},
	}
	for _, c := range cases {
		m := ""
		if c.mapped {
			m = mount
		}
		tp := newTitlePlanner(1, c.template, books, m)
		got := tp.plan(c.rec)
		tp.close()
		conflictOK := got.Conflict == c.conflict || (c.conflict == "*" && got.Conflict != "")
		if got.Path != c.path || got.Kind != c.kind || got.Siblings != c.siblings || got.State != c.state || !conflictOK {
			t.Errorf("%s: %+v\nwant path %q kind %s siblings %d state %s conflict %q", c.name, got, c.path, c.kind, c.siblings, c.state, c.conflict)
		}
	}
}

// countingFS counts the folders read below the plans' cache.
type countingFS struct {
	transfer.LibraryFS
	reads map[string]int
}

func (c *countingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := c.LibraryFS.ReadDir(name)
	if err == nil {
		c.reads[name]++
	}
	return entries, err
}

// TestPlanReadsEachFolderOnce: a poll's plans read each folder of the library
// at most once however many titles pass through it, and only its root (where
// an author's folder under a former name would be, bridge#47) for a title
// whose folders don't exist.
func TestPlanReadsEachFolderOnce(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mount, "Mara Quill", "Orchard Cycle"), 0o755); err != nil {
		t.Fatal(err)
	}
	tp := newTitlePlanner(1, "", goldenBooks(), mount)
	defer tp.close()
	counted := &countingFS{LibraryFS: transfer.RootFS(tp.root), reads: map[string]int{}}
	tp.fsys = newDirCache(counted)

	tp.plan(rec("B0PLAN0020", "Nowhere Yet", "Nobody Known", "", 0))
	if len(counted.reads) != 1 || counted.reads["."] != 1 {
		t.Fatalf("a title in folders that don't exist read %v, want the root once", counted.reads)
	}
	for i, title := range []string{"The Copper Grove", "The Fourth Bough", "The Fifth Bough"} {
		if p := tp.plan(rec("B0PLAN003"+string(rune('0'+i)), title, "Mara Quill", "Orchard Cycle", float64(i+2))); p.State != link.PlanJoins {
			t.Fatalf("%s: %+v", title, p)
		}
	}
	for dir, n := range counted.reads {
		if n != 1 {
			t.Errorf("%s read %d times", dir, n)
		}
	}
	if counted.reads["."] != 1 || counted.reads["Mara Quill"] != 1 {
		t.Errorf("reads = %v", counted.reads)
	}
}

// TestPollPlansTheUnmatched: each unmatched title of a poll goes out with
// where a backup would go under the account's template (the poll's params),
// checked at the library's mount when it is mapped there; a matched title has
// no plan.
func TestPollPlansTheUnmatched(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mount, "Library", "Ada Fenwick", "Taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "Library", "Ada Fenwick", "Taken", "Taken.m4b"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{pageSize: 500, books: goldenBooks()}
	accts := &stubAccounts{acct: account(), recs: []source.Record{
		rec("B0PLAN0101", "The Glass Orchard", "Mara Quill", "Orchard Cycle", 1),
		rec("B0PLAN0102", "Taken", "Ada Fenwick", "", 0),
		rec("B0PLAN0103", "Lone Star", "New Writer", "", 0),
	}}
	h := setup(t, fs, accts)
	h.o.Libraries = &stubLibs{known: true, lib: link.Library{ID: 1, Name: "Books", Root: "/srv/books"},
		status: link.LibraryStatus{LibraryID: 1, MountPath: mount, Mapped: true}} // read-only is enough to plan
	h.o.Mount = func(int64, string) (string, bool) { return mount, true }

	const tmpl = "Library/{Author}/{Title}"
	params, _ := json.Marshal(Params{Account: "acct0001", Template: tmpl})
	if _, err := runParams(t, h, params); err != nil {
		t.Fatal(err)
	}
	byASIN := map[string]link.AudibleTitle{}
	for _, tt := range fs.calls[0].Titles {
		byASIN[tt.ASIN] = tt
	}
	if tt := byASIN["B0PLAN0101"]; tt.Match == nil || tt.Planned != nil {
		t.Errorf("matched: %+v", tt)
	}
	want := map[string]link.AudiblePlan{
		"B0PLAN0102": {LibraryID: 1, Path: "Library/Ada Fenwick/Taken/Taken.m4b", File: "Taken.m4b", Kind: link.PlanStandalone,
			State: link.PlanExists, Conflict: link.ConflictFile, Template: tmpl},
		"B0PLAN0103": {LibraryID: 1, Path: "Library/New Writer/Lone Star/Lone Star.m4b", File: "Lone Star.m4b", Kind: link.PlanStandalone,
			State: link.PlanJoins, Template: tmpl}, // "Library" is a folder at the mount
	}
	for asin, w := range want {
		if p := byASIN[asin].Planned; p == nil || *p != w {
			t.Errorf("%s planned %+v, want %+v", asin, p, w)
		}
	}

	// Not mapped at the Bridge (no check at this mount): the books listing only.
	h.o.Libraries = &stubLibs{known: true, lib: link.Library{ID: 1, Name: "Books", Root: "/srv/books"},
		status: link.LibraryStatus{LibraryID: 1, MountPath: mount, Mapped: false}}
	if _, err := runParams(t, h, params); err != nil {
		t.Fatal(err)
	}
	for _, tt := range fs.calls[1].Titles {
		if tt.ASIN == "B0PLAN0102" && (tt.Planned == nil || tt.Planned.State != link.PlanNew || tt.Planned.Conflict != "") {
			t.Errorf("unmapped: %+v", tt.Planned)
		}
	}
}

// TestPollParamsTemplate: the poll's params take the account's template
// (absent is Auto), strictly, and bounded.
func TestPollParamsTemplate(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]Params{
		`{"account":"acct0001"}`:                         {Account: "acct0001"},
		`{"account":"acct0001","template":""}`:           {Account: "acct0001"},
		`{"account":"acct0001","template":"{Author}/x"}`: {Account: "acct0001", Template: "{Author}/x"},
	} {
		if got, err := ParseParams(json.RawMessage(raw)); err != nil || got != want {
			t.Errorf("%s = %+v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		`{"account":"acct0001","template":"x","dest_path":"y"}`,
		`{"account":"acct0001","template":"` + strings.Repeat("x", maxTemplate+1) + `"}`,
		`{"account":"acct0001","template":7}`,
	} {
		if _, err := ParseParams(json.RawMessage(raw)); err == nil {
			t.Errorf("%.80s accepted", raw)
		}
	}
	// Absent stays absent on the wire.
	if b, _ := json.Marshal(Params{Account: "acct0001"}); string(b) != `{"account":"acct0001"}` {
		t.Errorf("params = %s", b)
	}
}

func runParams(t *testing.T, h *Handler, params json.RawMessage) (Result, error) {
	t.Helper()
	out, err := h.Run(context.Background(), jobs.Job{ID: 8, Kind: Kind, Params: params, Attempt: 1}, jobs.LocalReporter())
	if err != nil {
		return Result{}, err
	}
	return out.(Result), nil
}
