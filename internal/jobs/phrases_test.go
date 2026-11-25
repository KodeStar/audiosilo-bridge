package jobs

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every Say constant has a phrase and every phrase a constant; codes are
// unique, wire-safe words (the console's locale keys bridge.progress.<code>).
func TestPhrasesComplete(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "phrases.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || !strings.HasPrefix(vs.Names[0].Name, "Say") {
			return true
		}
		var lit *ast.BasicLit
		if len(vs.Values) == 1 {
			lit, _ = vs.Values[0].(*ast.BasicLit)
		}
		if lit == nil {
			t.Fatalf("%s isn't a string constant", vs.Names[0].Name)
			return false
		}
		code, _ := strconv.Unquote(lit.Value)
		if codes[code] {
			t.Errorf("code %q is used twice", code)
		}
		codes[code] = true
		return true
	})
	word := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for code := range codes {
		if !word.MatchString(code) {
			t.Errorf("code %q isn't a snake_case word", code)
		}
		if strings.TrimSpace(phrases[code]) == "" {
			t.Errorf("code %q has no phrase", code)
		}
	}
	for code := range phrases {
		if !codes[code] {
			t.Errorf("phrase %q has no Say constant", code)
		}
	}
}

// The codes shipped are wire (the console's locale keys): none is renamed or
// dropped, and none changes its sentence unnoticed. A new code is added to
// testdata/phrases.json, and a reworded one changed there, with the console's
// bridge.progress.<code> locale keys.
func TestPhrasesShipped(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "phrases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shipped map[string]string
	if err := json.Unmarshal(raw, &shipped); err != nil {
		t.Fatal(err)
	}
	for code, tmpl := range shipped {
		switch got, ok := phrases[code]; {
		case !ok:
			t.Errorf("shipped code %q is gone: a code is never renamed or dropped", code)
		case got != tmpl:
			t.Errorf("code %q reads %q, shipped as %q: update testdata/phrases.json and the console's locales", code, got, tmpl)
		}
	}
	for code := range phrases {
		if _, ok := shipped[code]; !ok {
			t.Errorf("code %q isn't in testdata/phrases.json: add it (and the console's locale key)", code)
		}
	}
}

// LibraryNotWritable's reason is its phrase without the "Needs attention" lead.
func TestLibraryNotWritable(t *testing.T) {
	t.Parallel()
	params, why := LibraryNotWritable("Books")
	if params["library"] != "Books" || Phrase(SayLibraryNotWritable, params) != needsAttention+why ||
		!strings.HasPrefix(why, "Library Books must be mapped and writable") {
		t.Fatalf("params %v, reason %q", params, why)
	}
}

// Phrase fills a code's placeholders from params, leaves one params lacks, and
// is "" for a code it doesn't know.
func TestPhrase(t *testing.T) {
	t.Parallel()
	if got := Phrase(SayStarting, nil); got != "Starting" {
		t.Fatalf("plain = %q", got)
	}
	got := Phrase(SayDownloadingSection, map[string]any{"section": 3, "sections": 12})
	if got != "Downloading section 3 of 12 from archive.org" {
		t.Fatalf("filled = %q", got)
	}
	if got := Phrase(SayDownloadingSection, map[string]any{"section": 3}); !strings.Contains(got, "of {{sections}}") {
		t.Fatalf("missing param = %q", got)
	}
	if got := Phrase("no_such_code", nil); got != "" {
		t.Fatalf("unknown = %q", got)
	}
}

// A coded progress is sent with its English message (an older console shows
// it), its code and its params, with or without Extra; an Extra key never
// shadows them, and a message the handler wrote itself is kept.
func TestProgressCodeOnTheWire(t *testing.T) {
	t.Parallel()
	read := func(p Progress) map[string]any {
		t.Helper()
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	params := map[string]any{"library": "Books"}
	for _, extra := range []map[string]any{nil, {"message_code": "x", "message_params": "y", "park": map[string]any{"code": "not_writable"}}} {
		got := read(Progress{Step: "place", Code: SayLibraryNotWritable, Params: params, Extra: extra})
		sent, _ := got["message_params"].(map[string]any)
		msg, _ := got["message"].(string)
		if got["message_code"] != SayLibraryNotWritable || sent["library"] != "Books" ||
			!strings.HasPrefix(msg, "Needs attention: Library Books must be mapped") {
			t.Fatalf("extra %v: %v", extra, got)
		}
	}
	if got := read(Progress{Step: "x", Message: "Own words", Code: SayStarting}); got["message"] != "Own words" {
		t.Fatalf("own message = %v", got)
	}
	if got := read(Progress{Step: "x", Message: "Plain"}); got["message_code"] != nil || got["message_params"] != nil {
		t.Fatalf("an uncoded progress names a code: %v", got)
	}
}

// Params past the server's cap (they are never clipped, as message is) go, with
// Extra, rather than have the server refuse the progress for good: the code and
// the clipped English message still go.
func TestProgressParamsPastTheCap(t *testing.T) {
	t.Parallel()
	rep := LocalReporter()
	params, _ := LibraryNotWritable(strings.Repeat("x", maxProgressBytes))
	rep.Progress(Progress{Step: "place", Code: SayLibraryNotWritable, Params: params,
		Extra: map[string]any{"park": map[string]any{"code": "not_writable"}}})
	b, err := json.Marshal(rep.Last())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	msg, _ := got["message"].(string)
	if len(b) > maxProgressBytes || got["message_params"] != nil || got["message_code"] != SayLibraryNotWritable ||
		len(msg) != maxMessageText {
		t.Fatalf("%d bytes: code %v, params sent %v, message %d bytes", len(b), got["message_code"],
			got["message_params"] != nil, len(msg))
	}
}

// A stored coded progress reads back with its code and params as fields, not
// Extra (a restarted job resends them as they were).
func TestDecodeProgressKeepsCode(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(Progress{Step: "download", Code: SayDownloadingSection,
		Params: map[string]any{"section": 1, "sections": 2}, Extra: map[string]any{"source": "audio"}})
	p := decodeProgress(raw)
	if p.Code != SayDownloadingSection || p.Params["sections"] != float64(2) || p.Message == "" {
		t.Fatalf("decoded = %+v", p)
	}
	if _, ok := p.Extra["message_code"]; ok || p.Extra["source"] != "audio" || len(p.Extra) != 1 {
		t.Fatalf("extra = %v", p.Extra)
	}
}

// The reporter records a coded progress with its English message filled in.
func TestReporterFillsMessage(t *testing.T) {
	t.Parallel()
	rep := LocalReporter()
	rep.Progress(Progress{Step: "stat", Code: SayComparingFiles, Total: 3})
	if p := rep.Last(); p.Message != "Comparing the files" || p.Code != SayComparingFiles {
		t.Fatalf("last = %+v", p)
	}
}
