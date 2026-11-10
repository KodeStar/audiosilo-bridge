package audiblejob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The secrets a license carries in these tests: none may reach a log, a
// progress, a result, an error or a file the job leaves.
const (
	secretKey = "5ec7e75ec7e75ec7e75ec7e75ec7e7aa"
	secretIV  = "1b1b1b1b1b1b1b1b1b1b1b1b1b1b1bbb"
	secretSig = "SECRETSIG"
)

type stubImportAccounts struct {
	mu       sync.Mutex
	acct     store.AudibleAccount
	acctThen []store.AudibleAccount // each Account read after the first answers the next
	acctErr  error
	title    source.Record
	titleErr error
	licErr   []error // popped per License call
	licenses int
	body     []byte
	// failNext: each Download pops one; a non-nil error writes half of what is
	// left first (a dropped connection), or nothing when dropEmpty.
	failNext  []error
	dropEmpty bool
	// download replaces the stub's Download when set.
	download  func(ctx context.Context, url, dest string, opts audible.DownloadOptions) error
	downloads int
}

func (s *stubImportAccounts) Account(_ context.Context, id string) (store.AudibleAccount, error) {
	if s.acctErr != nil {
		return store.AudibleAccount{}, s.acctErr
	}
	if id != s.acct.ID {
		return store.AudibleAccount{}, audibleacct.ErrNotFound
	}
	a := s.acct
	if len(s.acctThen) > 0 {
		s.acct, s.acctThen = s.acctThen[0], s.acctThen[1:]
	}
	return a, nil
}

func (s *stubImportAccounts) Title(context.Context, string, string) (source.Record, error) {
	return s.title, s.titleErr
}

func (s *stubImportAccounts) License(context.Context, string, string) (audible.License, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.licenses++
	if len(s.licErr) > 0 {
		err := s.licErr[0]
		s.licErr = s.licErr[1:]
		if err != nil {
			return audible.License{}, err
		}
	}
	return audible.License{URL: fmt.Sprintf("https://cdn.example/f%d?Signature=%s", s.licenses, secretSig), Key: secretKey, IV: secretIV,
		Size: int64(len(s.body))}, nil
}

func (s *stubImportAccounts) Download(ctx context.Context, _ string, url, dest string, opts audible.DownloadOptions) error {
	s.mu.Lock()
	s.downloads++
	s.mu.Unlock()
	if s.download != nil {
		return s.download(ctx, url, dest, opts)
	}
	var held int64
	if st, err := os.Stat(dest); err == nil {
		held = st.Size()
	}
	if opts.BeforeWrite != nil {
		if err := opts.BeforeWrite(int64(len(s.body)), held); err != nil {
			return err
		}
	}
	rest := s.body[held:]
	var fail error
	if len(s.failNext) > 0 {
		fail, s.failNext = s.failNext[0], s.failNext[1:]
	}
	if fail != nil {
		rest = rest[:len(rest)/2]
		if s.dropEmpty {
			rest = nil
		}
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, _ = f.Write(rest)
	_ = f.Close()
	if opts.OnProgress != nil {
		opts.OnProgress(held+int64(len(rest)), int64(len(s.body)))
	}
	return fail
}

type stubImportServer struct {
	mu        sync.Mutex
	books     []link.LibraryBook
	rescanErr []error
	rescanned []string
	enriched  []string
	enrichErr error
	// enrichErrs answer the enrichment PUTs in turn before enrichErr does.
	enrichErrs []error
	// bookOf is the book the rescan answers for a file (nil: its folder).
	bookOf func(p string) string
}

func (s *stubImportServer) LibraryBooks(_ context.Context, _ int64, _ string, _ int) (*link.LibraryBooks, error) {
	return &link.LibraryBooks{Books: s.books}, nil
}

func (s *stubImportServer) Rescan(_ context.Context, _ int64, p string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rescanned = append(s.rescanned, p)
	if len(s.rescanErr) > 0 {
		err := s.rescanErr[0]
		s.rescanErr = s.rescanErr[1:]
		if err != nil {
			return "", err
		}
	}
	if s.bookOf != nil {
		return s.bookOf(p), nil
	}
	return path.Dir(p), nil
}

func (s *stubImportServer) Enrich(_ context.Context, _ int64, p, asin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enriched = append(s.enriched, p+"="+asin)
	if len(s.enrichErrs) > 0 {
		err := s.enrichErrs[0]
		s.enrichErrs = s.enrichErrs[1:]
		return err
	}
	return s.enrichErr
}

type stubLibs struct {
	lib    link.Library
	status link.LibraryStatus
	known  bool
}

func (s *stubLibs) Library(id int64) (link.Library, bool) { return s.lib, s.known && id == s.lib.ID }
func (s *stubLibs) Status(id int64) (link.LibraryStatus, bool) {
	return s.status, s.known && id == s.lib.ID && s.status.MountPath != ""
}

type importEnv struct {
	root, tmp string
	accts     *stubImportAccounts
	srv       *stubImportServer
	libs      *stubLibs
	logs      *bytes.Buffer
	opts      ImportOptions
	steps     []string
	progress  []jobs.Progress
	decrypted []audible.License
	waits     []time.Duration
	mu        sync.Mutex
}

func newImportEnv(t *testing.T) *importEnv {
	t.Helper()
	e := &importEnv{root: t.TempDir(), tmp: filepath.Join(t.TempDir(), "audible", "tmp"), logs: &bytes.Buffer{}}
	e.accts = &stubImportAccounts{
		acct:  store.AudibleAccount{ID: "acct0001", LibraryID: 1, ServerID: "srv1"},
		title: rec("B0FAKE0002", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2),
		body:  bytes.Repeat([]byte("audiobook-bytes-"), 4096),
	}
	e.srv = &stubImportServer{books: goldenBooks()}
	e.libs = &stubLibs{known: true, lib: link.Library{ID: 1, Name: "Books", Root: "/srv/books"},
		status: link.LibraryStatus{LibraryID: 1, MountPath: e.root, Mapped: true, Writable: true}}
	e.opts = ImportOptions{
		Accounts: e.accts, ServerID: func() string { return "srv1" }, Libraries: e.libs,
		Server: func() ImportServer { return e.srv },
		Mount:  func(int64, string) (string, bool) { return e.root, true },
		TmpDir: e.tmp, Log: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Decrypt: func(_ context.Context, _, in, out string, lic audible.License) error {
			e.mu.Lock()
			e.decrypted = append(e.decrypted, lic)
			e.mu.Unlock()
			b, err := os.ReadFile(in)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o600)
		},
		Free: func(string) (int64, error) { return 1 << 40, nil },
		Wait: func(ctx context.Context, d time.Duration) error {
			e.mu.Lock()
			e.waits = append(e.waits, d)
			e.mu.Unlock()
			return ctx.Err()
		},
		onProgress: func(p jobs.Progress) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.progress = append(e.progress, p)
			if len(e.steps) == 0 || e.steps[len(e.steps)-1] != p.Step {
				e.steps = append(e.steps, p.Step)
			}
		},
	}
	return e
}

func (e *importEnv) params(f func(*ImportParams)) json.RawMessage {
	p := ImportParams{Account: "acct0001", ASIN: "B0FAKE0002", LibraryID: 1}
	if f != nil {
		f(&p)
	}
	b, _ := json.Marshal(p)
	return b
}

func (e *importEnv) run(ctx context.Context, params json.RawMessage) (any, error) {
	return NewImport(e.opts).Run(ctx, jobs.Job{ID: 7, Kind: ImportKind, Params: params, Attempt: 1}, jobs.LocalReporter())
}

// resumed runs the job as the runner resumes one after a restart, reporting
// to rep.
func (e *importEnv) resumed(ctx context.Context, params json.RawMessage, rep *jobs.Reporter) (any, error) {
	return NewImport(e.opts).Run(ctx, jobs.Job{ID: 7, Kind: ImportKind, Params: params, Attempt: 1, Resumed: true}, rep)
}

// parks are the park codes the job's progress named, with their steps.
func (e *importEnv) parks() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, p := range e.progress {
		if park, ok := p.Extra["park"].(map[string]any); ok {
			out = append(out, p.Step+":"+fmt.Sprint(park["code"]))
		}
	}
	return out
}

// said reports whether a progress of the job said msg.
func (e *importEnv) said(msg string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.progress {
		if strings.Contains(p.Message, msg) {
			return true
		}
	}
	return false
}

// keepPartial leaves a partial download of the title in the temp folder (and
// its state), as a stopped run of it does, and returns its path.
func (e *importEnv) keepPartial(t *testing.T) string {
	t.Helper()
	part := filepath.Join(e.tmp, "acct0001-B0FAKE0002.aaxc.part")
	if err := os.MkdirAll(e.tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, e.accts.body[:100], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audible.StatePath(part), []byte(`{"size":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return part
}

// kept fails unless the partial at part and its state are still there.
func kept(t *testing.T, part string) {
	t.Helper()
	for _, p := range []string{part, audible.StatePath(part)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
	}
}

// noSecrets fails when a license's key, iv or URL signature shows anywhere
// the job leaves words: its logs, progress, result, error, or a file in the
// temp folder or the library.
func (e *importEnv) noSecrets(t *testing.T, result any, err error) {
	t.Helper()
	var texts []string
	texts = append(texts, e.logs.String())
	if b, merr := json.Marshal(result); merr == nil {
		texts = append(texts, string(b))
	}
	if err != nil {
		texts = append(texts, err.Error())
	}
	pb, _ := json.Marshal(e.progress)
	texts = append(texts, string(pb))
	for _, dir := range []string{e.tmp, e.root} {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
			if werr == nil && d.Type().IsRegular() {
				b, _ := os.ReadFile(p)
				texts = append(texts, string(b))
			}
			return nil
		})
	}
	for _, s := range texts {
		for _, secret := range []string{secretKey, secretIV, secretSig} {
			if strings.Contains(strings.ToLower(s), strings.ToLower(secret)) {
				t.Fatalf("a secret leaked: %.300s", s)
			}
		}
	}
}

// tmpEmpty fails when the import left anything in its temp folder.
func (e *importEnv) tmpEmpty(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(e.tmp)
	if len(entries) != 0 {
		t.Fatalf("temp files left: %v", entries)
	}
}

// writeLibrary puts a file at rel in the library (an administrator's).
func (e *importEnv) writeLibrary(rel, body string) {
	full := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil { //nolint:gosec // a test's library file
		panic(err)
	}
}

// libraryFiles lists the library's files (relative).
func (e *importEnv) libraryFiles() []string {
	var out []string
	_ = filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(e.root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

const copperGrove = "Mara Quill/Orchard Cycle/OC02 - The Copper Grove/The Copper Grove.m4b"

func TestImportBacksATitleUp(t *testing.T) {
	e := newImportEnv(t)
	res, err := e.run(context.Background(), e.params(nil))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.(*ImportResult)
	if !ok {
		t.Fatalf("result = %#v", res)
	}
	size := int64(len(e.accts.body))
	want := ImportResult{LibraryID: 1, Path: path.Dir(copperGrove), Files: []ImportFile{{Path: copperGrove, Size: size}}, Bytes: size, Enriched: true}
	if b1, b2 := mustJSON(t, got), mustJSON(t, want); b1 != b2 {
		t.Fatalf("result = %s\nwant %s", b1, b2)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(copperGrove))); !bytes.Equal(b, e.accts.body) {
		t.Fatal("the library's file isn't the book")
	}
	if strings.Join(e.steps, ",") != "license,download,decrypt,place,rescan,enrich" {
		t.Fatalf("steps = %v", e.steps)
	}
	if len(e.srv.rescanned) != 1 || e.srv.rescanned[0] != copperGrove || len(e.srv.enriched) != 1 ||
		e.srv.enriched[0] != path.Dir(copperGrove)+"=B0FAKE0002" {
		t.Fatalf("rescanned %v, enriched %v", e.srv.rescanned, e.srv.enriched)
	}
	if len(e.decrypted) != 1 || e.decrypted[0].Key != secretKey || e.decrypted[0].IV != secretIV {
		t.Fatal("decrypt didn't get the license's key and iv")
	}
	e.tmpEmpty(t)
	e.noSecrets(t, res, err)

	// The result's wire shape (the server reads it).
	var wire map[string]any
	_ = json.Unmarshal([]byte(mustJSON(t, got)), &wire)
	for _, k := range []string{"library_id", "path", "files", "bytes", "enriched"} {
		if _, ok := wire[k]; !ok {
			t.Fatalf("result lacks %q: %v", k, wire)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A server that couldn't take the ASIN (another one is set) is no failure.
func TestImportEnrichRefusedIsNotAFailure(t *testing.T) {
	e := newImportEnv(t)
	e.srv.enrichErr = link.ErrASINSet
	res, err := e.run(context.Background(), e.params(nil))
	if err != nil || res.(*ImportResult).Enriched {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

// A failure's library path and title stay whole (the redactor's shapes would
// read "Basic Economics" as a Basic credential, "Password: A Novel" as a
// password), while an error it quotes is still redacted of Amazon's ids.
func TestFailRedactsOnlyQuotedErrors(t *testing.T) {
	t.Parallel()
	const rel = "Thomas Sowell/Basic Economics/Basic Economics.m4b"
	if f := fail(CodeExists, "A file is already at %s: %q", rel, "Password: A Novel"); f.Message !=
		`A file is already at `+rel+`: "Password: A Novel"` {
		t.Fatalf("message = %s", f.Message)
	}
	f := fail(CodeNoRights, "At %s: %v", rel, errors.New(amazonRefusal))
	if strings.Contains(f.Message, "A1B2C3D4E5F6G7") || !strings.Contains(f.Message, rel) ||
		!strings.Contains(f.Message, "being throttled") {
		t.Fatalf("message = %s", f.Message)
	}
}

// Denied: Amazon's text reaching the import unredacted (belt and braces: the
// client redacts it first) leaves the customer id out of the job's error,
// result, progress and logs, as a coded failure and as a plain error; the ASIN
// and the reason stay.
func TestImportErrorsRedactAccountIDs(t *testing.T) {
	for name, licErr := range map[string]error{
		"coded":   fmt.Errorf("%w: B0FAKE0002 (%s)", audible.ErrNoRights, amazonRefusal),
		"uncoded": errors.New(amazonRefusal),
	} {
		t.Run(name, func(t *testing.T) {
			e := newImportEnv(t)
			e.accts.licErr = []error{licErr}
			res, err := e.run(context.Background(), e.params(nil))
			if err == nil {
				t.Fatal("expected a failure")
			}
			pb, _ := json.Marshal(e.progress)
			for _, s := range []string{err.Error(), mustJSON(t, res), string(pb), e.logs.String()} {
				if strings.Contains(s, "A1B2C3D4E5F6G7") {
					t.Fatalf("customer id leaked: %.400s", s)
				}
			}
			if msg := err.Error(); !strings.Contains(msg, "B00H3R0B0W") || !strings.Contains(msg, "being throttled") {
				t.Fatalf("the reason was lost: %s", msg)
			}
		})
	}
}

// Each refusal answers its code, leaves nothing in the library or the temp
// folder, and says nothing secret.
func TestImportCodes(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *importEnv)
		params func(*ImportParams)
		code   string
	}{
		{name: "unknown account", setup: func(e *importEnv) { e.accts.acctErr = audibleacct.ErrNotFound }, code: CodeUnknownAccount},
		{name: "another server's", setup: func(e *importEnv) { e.accts.acct.ServerID = "srv0" }, code: CodeUnknownAccount},
		{name: "not the account's library", params: func(p *ImportParams) { p.LibraryID = 2 }, code: CodeNotWritable},
		{name: "not writable", setup: func(e *importEnv) { e.libs.status.Writable = false }, code: CodeNotWritable},
		{name: "not mapped", setup: func(e *importEnv) { e.libs.status.Mapped = false }, code: CodeNotWritable},
		{name: "a podcast library", setup: func(e *importEnv) { e.libs.lib.Kind = "podcasts" }, code: CodeNotWritable},
		{name: "unlisted library", setup: func(e *importEnv) { e.libs.known = false }, code: CodeNotWritable},
		{name: "preorder", setup: func(e *importEnv) {
			e.accts.title.Preorder, e.accts.title.UnavailableReason = true, "pre-order"
		}, code: CodeNotReleased},
		{name: "no rights", setup: func(e *importEnv) { e.accts.title.UnavailableReason = "no download rights" }, code: CodeNoRights},
		{name: "not in the library", setup: func(e *importEnv) { e.accts.titleErr = audible.ErrNotInLibrary }, code: CodeNoRights},
		{name: "sign-in refused", setup: func(e *importEnv) { e.accts.titleErr = audibleacct.ErrRefused }, code: CodeRefused},
		{name: "license: not released", setup: func(e *importEnv) {
			e.accts.licErr = []error{fmt.Errorf("%w: B0FAKE0002 (Release NotReleased)", audible.ErrNotReleased)}
		}, code: CodeNotReleased},
		{name: "license: no rights", setup: func(e *importEnv) {
			e.accts.licErr = []error{fmt.Errorf("%w: B0FAKE0002 (Ownership)", audible.ErrNoRights)}
		}, code: CodeNoRights},
		{name: "license: denied", setup: func(e *importEnv) {
			e.accts.licErr = []error{fmt.Errorf("%w: B0FAKE0002 (Throttled)", audible.ErrDenied)}
		}, code: CodeRefused},
		{name: "license: refused", setup: func(e *importEnv) { e.accts.licErr = []error{audibleacct.ErrRefused} }, code: CodeRefused},
		// A device on an older profile: the account's to fix, not the title's.
		{name: "license: throttled device", setup: func(e *importEnv) {
			e.accts.licErr = []error{fmt.Errorf("%w: B0FAKE0002 (%s)", audible.ErrThrottled, amazonRefusal)}
		}, code: CodeRefused},
		{name: "exists", setup: func(e *importEnv) {
			p := filepath.Join(e.root, filepath.FromSlash(copperGrove))
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte("an administrator's own copy"), 0o644)
		}, code: CodeExists},
		{name: "dest escapes", params: func(p *ImportParams) { p.DestPath = "../../etc" }, code: CodeNotWritable},
		{name: "temp space short", setup: func(e *importEnv) {
			e.opts.Free = func(dir string) (int64, error) {
				if dir == e.tmp {
					return int64(len(e.accts.body)), nil // under 2.2x
				}
				return 1 << 40, nil
			}
		}, code: CodeDiskFull},
		{name: "library space short", setup: func(e *importEnv) {
			e.opts.Free = func(dir string) (int64, error) {
				if dir == e.tmp {
					return 1 << 40, nil
				}
				return int64(len(e.accts.body)), nil // under 1.1x
			}
		}, code: CodeDiskFull},
		{name: "decrypt: out of quota", setup: func(e *importEnv) {
			e.opts.Decrypt = func(context.Context, string, string, string, audible.License) error {
				return errors.New("exit status 1: [out#0/ipod] Error writing trailer: Disk quota exceeded")
			}
		}, code: CodeDiskFull},
		{name: "dest names another audio file", params: func(p *ImportParams) { p.DestPath = "Imports/Copper.mp3" }, code: CodeNotWritable},
		{name: "dest in a hidden folder", params: func(p *ImportParams) { p.DestPath = "Imports/.audible" }, code: CodeNotWritable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newImportEnv(t)
			if c.setup != nil {
				c.setup(e)
			}
			res, err := e.run(context.Background(), e.params(c.params))
			var f *Failure
			if !errors.As(err, &f) || f.Code != c.code || res != any(f) {
				t.Fatalf("res %#v, err %v; want code %s", res, err, c.code)
			}
			// The failure's wire shape: the server reads its "code".
			var wire map[string]any
			_ = json.Unmarshal([]byte(mustJSON(t, res)), &wire)
			if msg, _ := wire["message"].(string); wire["code"] != c.code || msg != err.Error() || len(wire) != 2 {
				t.Fatalf("failure on the wire = %v", wire)
			}
			files := e.libraryFiles()
			if c.code == CodeExists {
				if len(files) != 1 {
					t.Fatalf("library = %v", files)
				}
				if b, _ := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(copperGrove))); string(b) != "an administrator's own copy" {
					t.Fatal("the existing file was changed")
				}
			} else if len(files) != 0 {
				t.Fatalf("a refused import wrote %v", files)
			}
			e.tmpEmpty(t)
			e.noSecrets(t, res, err)
		})
	}
}

// A refused account's import fails before asking Audible anything: a device
// denied licenses with the throttled advice, a refused sign-in with its own.
// Allowed: an account with only a poll's error still imports.
func TestImportOfARefusedAccountFailsFast(t *testing.T) {
	for _, c := range []struct {
		name, kind, why, msg string
	}{
		{"throttled", store.RefusalDevice, audibleacct.ThrottledText, ThrottledText},
		{"sign-in refused", store.RefusalSignIn, audibleacct.RefusedText, refusedSignIn().Message},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newImportEnv(t)
			e.accts.acct.RefusedAt, e.accts.acct.LastError, e.accts.acct.RefusalKind = "2026-11-03T10:00:00Z", c.why, c.kind
			_, err := e.run(context.Background(), e.params(nil))
			var f *Failure
			if !errors.As(err, &f) || f.Code != CodeRefused || f.Message != c.msg {
				t.Fatalf("err = %v", err)
			}
			if e.accts.licenses != 0 || e.accts.downloads != 0 {
				t.Fatalf("licenses %d, downloads %d", e.accts.licenses, e.accts.downloads)
			}
			e.tmpEmpty(t)
		})
	}
	// A sign-in again that ended the refusal after the job's first read
	// (while it waited for the library's mapping) imports.
	e := newImportEnv(t)
	signedIn := e.accts.acct
	e.accts.acct.RefusedAt, e.accts.acct.LastError, e.accts.acct.RefusalKind = "2026-11-03T10:00:00Z", audibleacct.ThrottledText, store.RefusalDevice
	e.accts.acctThen = []store.AudibleAccount{signedIn}
	if _, err := e.run(context.Background(), e.params(nil)); err != nil || e.accts.licenses != 1 {
		t.Fatalf("an account signed in again meanwhile: %v (licenses %d)", err, e.accts.licenses)
	}

	e = newImportEnv(t)
	e.accts.acct.LastError = "could not list the Audible library: connection reset"
	if _, err := e.run(context.Background(), e.params(nil)); err != nil || e.accts.licenses != 1 {
		t.Fatalf("an account with a poll error: %v (licenses %d)", err, e.accts.licenses)
	}
}

// Bad params fail before anything is asked of Audible.
func TestImportParams(t *testing.T) {
	for _, raw := range []string{`{}`, `{"account":"BAD","asin":"B0FAKE0002","library_id":1}`,
		`{"account":"acct0001","asin":"nope","library_id":1}`, `{"account":"acct0001","asin":"B0FAKE0002"}`, `[]`} {
		if _, err := ParseImportParams(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
	// Unknown fields are the server's to add; the ASIN is read upper-case.
	p, err := ParseImportParams(json.RawMessage(`{"account":"acct0001","asin":"b0fake0002","library_id":1,"later":true}`))
	if err != nil || p.ASIN != "B0FAKE0002" {
		t.Fatalf("p = %+v, %v", p, err)
	}
	e := newImportEnv(t)
	lib := int64(9)
	if _, err := NewImport(e.opts).Run(context.Background(), jobs.Job{Kind: ImportKind, LibraryID: &lib, Params: e.params(nil)}, jobs.LocalReporter()); err == nil {
		t.Fatal("a job library other than the params' ran")
	}
	if e.accts.licenses != 0 {
		t.Fatal("bad params asked Audible for a license")
	}
}

// A dropped connection resumes from the partial within the job, after a
// wait; the file comes out whole.
func TestImportResumesAfterADroppedConnection(t *testing.T) {
	e := newImportEnv(t)
	e.accts.failNext = []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}
	res, err := e.run(context.Background(), e.params(nil))
	if err != nil {
		t.Fatal(err)
	}
	if e.accts.downloads != 3 || len(e.waits) != 2 || e.accts.licenses != 1 {
		t.Fatalf("downloads %d, waits %v, licenses %d", e.accts.downloads, e.waits, e.accts.licenses)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(copperGrove))); !bytes.Equal(b, e.accts.body) {
		t.Fatal("the resumed book isn't whole")
	}
	e.noSecrets(t, res, err)
}

// A connection that keeps dropping with nothing new fails the job, and its
// partial goes.
func TestImportGivesUpAfterThreeDrops(t *testing.T) {
	e := newImportEnv(t)
	e.accts.dropEmpty = true
	e.accts.failNext = []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}
	if _, err := e.run(context.Background(), e.params(nil)); err == nil {
		t.Fatal("three drops in a row must fail")
	}
	if e.accts.downloads != 3 {
		t.Fatalf("downloads = %d, want 3", e.accts.downloads)
	}
	e.tmpEmpty(t)
	if len(e.libraryFiles()) != 0 {
		t.Fatal("a failed import wrote to the library")
	}
}

// Drops that each add to the partial don't use the attempts up: a long
// download over a link that drops now and then finishes.
func TestImportKeepsResumingWhileItProgresses(t *testing.T) {
	e := newImportEnv(t)
	for range 6 {
		e.accts.failNext = append(e.accts.failNext, io.ErrUnexpectedEOF)
	}
	res, err := e.run(context.Background(), e.params(nil))
	if err != nil {
		t.Fatal(err)
	}
	if e.accts.downloads != 7 || e.accts.licenses != 1 {
		t.Fatalf("downloads %d, licenses %d", e.accts.downloads, e.accts.licenses)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(copperGrove))); !bytes.Equal(b, e.accts.body) {
		t.Fatal("the resumed book isn't whole")
	}
	e.noSecrets(t, res, err)
}

// signedDownloads routes the stub's Download through a real audible client
// against srv, so the job sees Audible's real errors (a 403 on an expired URL).
func signedDownloads(t *testing.T, e *importEnv, srv *httptest.Server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	cl, err := audible.NewClient(audible.Credentials{ADPToken: "ADP", DevicePrivateKey: keyPEM, DeviceType: "A2CZJZGLK2JJVM",
		DeviceSerial: "SER", CustomerID: "CUST", Marketplace: "us"}, audible.WithEndpoints(audible.Endpoints{API: srv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	e.accts.download = func(ctx context.Context, url, dest string, opts audible.DownloadOptions) error {
		url = srv.URL + "/" + path.Base(strings.SplitN(url, "?", 2)[0]) + "?Signature=" + secretSig
		return cl.Download(ctx, url, dest, opts)
	}
}

// An expired download URL gets one new license; the new URL works.
func TestImportRelicensesAnExpiredURLOnce(t *testing.T) {
	e := newImportEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/f1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "f", time.Unix(0, 0), bytes.NewReader(e.accts.body))
	}))
	defer srv.Close()
	signedDownloads(t, e, srv)
	res, err := e.run(context.Background(), e.params(nil))
	if err != nil {
		t.Fatal(err)
	}
	if e.accts.licenses != 2 {
		t.Fatalf("licenses = %d, want 2", e.accts.licenses)
	}
	e.noSecrets(t, res, err)
}

// Expired again after the new license: offline_url_expired.
func TestImportURLExpiredTwice(t *testing.T) {
	e := newImportEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	signedDownloads(t, e, srv)
	res, err := e.run(context.Background(), e.params(nil))
	var f *Failure
	if !errors.As(err, &f) || f.Code != CodeURLExpired || e.accts.licenses != 2 {
		t.Fatalf("err = %v, licenses %d", err, e.accts.licenses)
	}
	e.tmpEmpty(t)
	e.noSecrets(t, res, err)
}

// blockingDownload writes half the book and waits for the job's end.
func blockingDownload(e *importEnv, started chan<- struct{}) func(ctx context.Context, url, dest string, opts audible.DownloadOptions) error {
	return func(ctx context.Context, _, dest string, _ audible.DownloadOptions) error {
		if err := os.WriteFile(dest, e.accts.body[:len(e.accts.body)/2], 0o600); err != nil {
			return err
		}
		_ = os.WriteFile(audible.StatePath(dest), []byte(`{"size":1}`), 0o600)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
}

// An admin's cancel mid-download leaves no temp file and no library file.
func TestImportCancelLeavesNothing(t *testing.T) {
	e := newImportEnv(t)
	started := make(chan struct{})
	e.accts.download = blockingDownload(e, started)
	ctx, cancel := jobs.CancelContext(context.Background())
	go func() { <-started; cancel() }()
	if _, err := e.run(ctx, e.params(nil)); err == nil {
		t.Fatal("a cancelled import succeeded")
	}
	e.tmpEmpty(t)
	if files := e.libraryFiles(); len(files) != 0 {
		t.Fatalf("library = %v", files)
	}
}

// A Bridge stopping mid-download keeps the partial (and its state) for the
// next start to resume (decision 6); the start-up sweep keeps it too.
func TestImportStopKeepsThePartial(t *testing.T) {
	e := newImportEnv(t)
	started := make(chan struct{})
	e.accts.download = blockingDownload(e, started)
	ctx, stop := context.WithCancel(context.Background())
	go func() { <-started; stop() }()
	if _, err := e.run(ctx, e.params(nil)); err == nil {
		t.Fatal("a stopped import succeeded")
	}
	part := filepath.Join(e.tmp, "acct0001-B0FAKE0002.aaxc.part")
	if err := SweepTmp(e.tmp, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{part, audible.StatePath(part)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
	}
	// The runner ending it without running it (a cancel before it resumed)
	// removes it.
	NewImport(e.opts).Abandon(context.Background(), jobs.Job{Kind: ImportKind, Params: e.params(nil)})
	e.tmpEmpty(t)
}

func TestSweepTmp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("a-B0FAKE0001.aaxc.part", time.Hour)
	write("a-B0FAKE0001.aaxc.part.state", time.Hour)
	write("a-B0FAKE0002.aaxc.part", 8*24*time.Hour) // too old
	write("a-B0FAKE0002.aaxc.part.state", time.Hour)
	write("a-B0FAKE0003.aaxc.part.state", time.Hour) // no partial
	write("a-B0FAKE0001.m4b", time.Minute)           // a decrypted copy a crash left
	write("a-B0FAKE0004.placed.json", time.Hour)
	write("a-B0FAKE0005.placed.json", 8*24*time.Hour) // too old
	write("stray.txt", time.Minute)
	if err := os.MkdirAll(filepath.Join(dir, "audible-123"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SweepTmp(dir, now); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, ",") != "a-B0FAKE0001.aaxc.part,a-B0FAKE0001.aaxc.part.state,a-B0FAKE0004.placed.json" {
		t.Fatalf("left %v", left)
	}
	if err := SweepTmp(filepath.Join(dir, "missing"), now); err != nil {
		t.Fatalf("a missing folder: %v", err)
	}
}

// One import per account at a time: a second waits for the first. Every wait
// is bounded, and a failure releases the held download and cancels the runs,
// so the test fails fast rather than hanging.
func TestImportOnePerAccount(t *testing.T) {
	e := newImportEnv(t)
	// started is closed (never sent on: a dropped signal hung CI) by the first
	// download to begin.
	started := make(chan struct{})
	var startOnce sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); releaseAll() })
	var mu sync.Mutex
	running, most := 0, 0
	e.accts.download = func(_ context.Context, _, dest string, _ audible.DownloadOptions) error {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		startOnce.Do(func() { close(started) })
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return os.WriteFile(dest, e.accts.body, 0o600)
	}
	waiting := make(chan struct{})
	var once sync.Once
	progress := e.opts.onProgress
	e.opts.onProgress = func(p jobs.Progress) {
		progress(p)
		if strings.Contains(p.Message, "other backup") {
			once.Do(func() { close(waiting) })
		}
	}
	h := NewImport(e.opts)
	errs := make(chan error, 2)
	for _, asin := range []string{"B0FAKE0002", "B0FAKE0003"} {
		params := e.params(func(p *ImportParams) { p.ASIN = asin; p.DestPath = "Imports/" + asin })
		go func() {
			_, err := h.Run(ctx, jobs.Job{Kind: ImportKind, Params: params}, jobs.LocalReporter())
			errs <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no import started its download")
	}
	select {
	case <-waiting: // the second waits while the first downloads
	case <-time.After(10 * time.Second):
		t.Fatal("the second import never waited for the first")
	}
	releaseAll()
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("an import never finished")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Fatalf("%d imports of one account ran at once", most)
	}
}

// A rescan that fails once is tried again; one that keeps failing fails the
// job, saying the book is placed.
func TestImportRescanRetries(t *testing.T) {
	e := newImportEnv(t)
	e.srv.rescanErr = []error{errors.New("busy")}
	if _, err := e.run(context.Background(), e.params(nil)); err != nil {
		t.Fatal(err)
	}
	e2 := newImportEnv(t)
	e2.srv.rescanErr = []error{errors.New("busy"), errors.New("busy"), errors.New("busy")}
	_, err := e2.run(context.Background(), e2.params(nil))
	if err == nil || !strings.Contains(err.Error(), "is in the library") {
		t.Fatalf("err = %v", err)
	}
}

// An install that succeeded before the rescan failed: the retry finds the
// book its marker recorded and goes on from the rescan, with the same result
// and no second license or download.
func TestImportRetryAfterTheInstallFinishes(t *testing.T) {
	e := newImportEnv(t)
	e.srv.rescanErr = []error{errors.New("busy"), errors.New("busy"), errors.New("busy")}
	if _, err := e.run(context.Background(), e.params(nil)); err == nil {
		t.Fatal("the rescan failing must fail the job")
	}
	if _, err := os.Stat(filepath.Join(e.tmp, "acct0001-B0FAKE0002"+placedSuffix)); err != nil {
		t.Fatalf("no placement marker: %v", err)
	}
	if err := SweepTmp(e.tmp, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.steps = nil
	res, err := e.run(context.Background(), e.params(nil))
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(e.accts.body))
	want := ImportResult{LibraryID: 1, Path: path.Dir(copperGrove), Files: []ImportFile{{Path: copperGrove, Size: size}}, Bytes: size, Enriched: true}
	if mustJSON(t, res) != mustJSON(t, want) {
		t.Fatalf("result = %s", mustJSON(t, res))
	}
	if e.accts.downloads != 1 || e.accts.licenses != 1 {
		t.Fatalf("downloads %d, licenses %d: the retry fetched the book again", e.accts.downloads, e.accts.licenses)
	}
	if strings.Join(e.steps, ",") != "rescan,enrich" {
		t.Fatalf("retry steps = %v", e.steps)
	}
	e.tmpEmpty(t) // the marker went with the result
	e.noSecrets(t, res, err)
}

// A marker whose file was changed or replaced since is no reason to go on:
// anything else at the destination still fails exists.
func TestImportMarkerNeedsTheSameFile(t *testing.T) {
	for name, change := range map[string]func(p string) error{
		"rewritten": func(p string) error {
			if err := os.WriteFile(p, []byte("an administrator's own copy"), 0o644); err != nil {
				return err
			}
			old := time.Now().Add(-time.Hour)
			return os.Chtimes(p, old, old)
		},
		"touched": func(p string) error {
			later := time.Now().Add(time.Hour)
			return os.Chtimes(p, later, later)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newImportEnv(t)
			e.srv.rescanErr = []error{errors.New("busy"), errors.New("busy"), errors.New("busy")}
			if _, err := e.run(context.Background(), e.params(nil)); err == nil {
				t.Fatal("the rescan failing must fail the job")
			}
			if err := change(filepath.Join(e.root, filepath.FromSlash(copperGrove))); err != nil {
				t.Fatal(err)
			}
			_, err := e.run(context.Background(), e.params(nil))
			var f *Failure
			if !errors.As(err, &f) || f.Code != CodeExists {
				t.Fatalf("err = %v, want exists", err)
			}
			if len(e.srv.rescanned) != 3 || e.accts.downloads != 1 {
				t.Fatalf("rescanned %v, downloads %d", e.srv.rescanned, e.accts.downloads)
			}
		})
	}
}

// A resumed job whose library isn't writable parks not_writable and waits,
// keeping its partial (CLAUDE.md: a resumed write job parks and waits); once
// the mapping is back it runs on, unparked. A new job is refused at once
// (TestImportCodes).
func TestImportResumedParksUntilTheMappingIsBack(t *testing.T) {
	e := newImportEnv(t)
	part := e.keepPartial(t)
	e.libs.status.Writable = false
	looks := 0
	e.opts.Wait = func(ctx context.Context, d time.Duration) error {
		if d == mappingPoll {
			looks++
			kept(t, part) // waiting keeps what it holds
			if looks == 3 {
				e.libs.status.Writable = true
			}
		}
		return ctx.Err()
	}
	rep := jobs.LocalReporter()
	res, err := e.resumed(context.Background(), e.params(nil), rep)
	if err != nil {
		t.Fatal(err)
	}
	parks := e.parks()
	if looks != 3 || len(parks) != 3 || slices.ContainsFunc(parks, func(p string) bool { return p != StepLicense+":"+CodeNotWritable }) {
		t.Fatalf("looks %d, parks %v", looks, parks)
	}
	if park := rep.Last().Extra["park"]; park != nil {
		t.Fatalf("the job is still parked: %v", park)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(copperGrove))); !bytes.Equal(b, e.accts.body) || e.accts.downloads != 1 {
		t.Fatalf("the resumed book isn't whole (downloads %d)", e.accts.downloads)
	}
	e.tmpEmpty(t)
	e.noSecrets(t, res, err)
}

// The mapping is checked again before the copy: gone during the download, the
// job parks with its decrypted copy and waits, then places the book once it
// is back, without downloading it again.
func TestImportParksAtThePlaceStep(t *testing.T) {
	e := newImportEnv(t)
	decrypt := e.opts.Decrypt
	e.opts.Decrypt = func(ctx context.Context, ffmpeg, in, out string, lic audible.License) error {
		e.libs.status.Writable = false // the share went read-only meanwhile
		return decrypt(ctx, ffmpeg, in, out, lic)
	}
	e.opts.Wait = func(ctx context.Context, d time.Duration) error {
		if d == mappingPoll {
			e.libs.status.Writable = true
		}
		return ctx.Err()
	}
	rep := jobs.LocalReporter()
	res, err := NewImport(e.opts).Run(context.Background(), jobs.Job{ID: 7, Kind: ImportKind, Params: e.params(nil), Attempt: 1}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if parks := e.parks(); !slices.Equal(parks, []string{StepPlace + ":" + CodeNotWritable}) || e.accts.downloads != 1 {
		t.Fatalf("parks %v, downloads %d", parks, e.accts.downloads)
	}
	if park := rep.Last().Extra["park"]; park != nil {
		t.Fatalf("the job is still parked: %v", park)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(copperGrove))); !bytes.Equal(b, e.accts.body) {
		t.Fatal("the library's file isn't the book")
	}
	e.tmpEmpty(t)
	e.noSecrets(t, res, err)
}

// A job claimed before the library's mapping check has a result for its mount
// (the first claims after a start, or a mount changed since) waits for the
// check without parking, then runs.
func TestImportWaitsForTheMappingCheck(t *testing.T) {
	for name, unchecked := range map[string]string{"not checked yet": "", "checked at another mount": "/old/mount"} {
		t.Run(name, func(t *testing.T) {
			e := newImportEnv(t)
			e.libs.status.MountPath = unchecked
			e.opts.Wait = func(ctx context.Context, d time.Duration) error {
				if d == mappingPoll {
					e.libs.status.MountPath = e.root
				}
				return ctx.Err()
			}
			if _, err := e.run(context.Background(), e.params(nil)); err != nil {
				t.Fatal(err)
			}
			if !e.said("Waiting for the library's mapping check") || len(e.parks()) != 0 {
				t.Fatalf("parks %v", e.parks())
			}
		})
	}
}

// A job that ends before its download leaves no partial an earlier run kept:
// refused (the account gone, another library) or cancelled while it waits
// for its mapping. Stopped there, or its claim lost, the partial stays for
// the next run.
func TestImportEndingEarlyCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(e *importEnv)
		params func(*ImportParams)
		end    string // how the job's context ends in the mapping wait
		keep   bool
	}{
		{name: "the account gone", setup: func(e *importEnv) { e.accts.acctErr = audibleacct.ErrNotFound }},
		{name: "another library", params: func(p *ImportParams) { p.LibraryID = 2 }},
		{name: "cancelled waiting for the mapping", end: "cancel"},
		{name: "stopped waiting for the mapping", end: "stop", keep: true},
		{name: "lost waiting for the mapping", end: "lose", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			part := e.keepPartial(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			ctx, end := context.Background(), func() {}
			switch tc.end {
			case "cancel":
				ctx, end = jobs.CancelContext(ctx)
			case "stop":
				var stop context.CancelFunc
				ctx, stop = context.WithCancel(ctx)
				end = stop
			case "lose":
				ctx, end = jobs.LoseContext(ctx)
			}
			if tc.end != "" {
				e.libs.status.MountPath = "" // it waits for the check
				e.opts.Wait = func(ctx context.Context, _ time.Duration) error { end(); return ctx.Err() }
			}
			if _, err := e.resumed(ctx, e.params(tc.params), jobs.LocalReporter()); err == nil {
				t.Fatal("the job succeeded")
			}
			if tc.keep {
				kept(t, part)
			} else {
				e.tmpEmpty(t)
			}
			if e.accts.licenses != 0 {
				t.Fatal("the job asked Audible for a license")
			}
		})
	}
}

// A lost claim (the lease lapsed while the Bridge was down or cut off) keeps
// the partial: the server hands the job out again, and that claim resumes it.
// Abandon hears it the same way: a lost claim keeps it, a cancel removes it.
func TestImportLostClaimKeepsThePartial(t *testing.T) {
	e := newImportEnv(t)
	started := make(chan struct{})
	e.accts.download = blockingDownload(e, started)
	ctx, lose := jobs.LoseContext(context.Background())
	go func() { <-started; lose() }()
	if _, err := e.run(ctx, e.params(nil)); err == nil {
		t.Fatal("a lost import succeeded")
	}
	part := filepath.Join(e.tmp, "acct0001-B0FAKE0002.aaxc.part")
	kept(t, part)
	h := NewImport(e.opts)
	job := jobs.Job{Kind: ImportKind, Params: e.params(nil)}
	h.Abandon(jobs.AbandonContext(context.Background(), true), job)
	kept(t, part)
	h.Abandon(jobs.AbandonContext(context.Background(), false), job)
	e.tmpEmpty(t)
}

// failRescans makes the env's next run install the book and then fail its
// rescans, leaving the placement marker.
func failRescans(t *testing.T, e *importEnv) {
	t.Helper()
	e.srv.rescanErr = []error{errors.New("busy"), errors.New("busy"), errors.New("busy")}
	if _, err := e.run(context.Background(), e.params(nil)); err == nil {
		t.Fatal("the rescan failing must fail the job")
	}
}

// The placement marker is the request's: a job asking for another
// destination places the title there (the earlier copy stays where it was),
// and one whose destination is the earlier file finishes it.
func TestImportMarkerFollowsTheRequest(t *testing.T) {
	t.Run("another destination", func(t *testing.T) {
		e := newImportEnv(t)
		failRescans(t, e)
		res, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = "Imports/Copper" }))
		if err != nil {
			t.Fatal(err)
		}
		if got := res.(*ImportResult); got.Path != "Imports/Copper" || e.accts.downloads != 2 {
			t.Fatalf("result %s, downloads %d", mustJSON(t, got), e.accts.downloads)
		}
		if files := e.libraryFiles(); !slices.Equal(files, []string{"Imports/Copper/The Copper Grove.m4b", copperGrove}) {
			t.Fatalf("library = %v", files)
		}
		e.tmpEmpty(t)
	})
	t.Run("the same file by another request", func(t *testing.T) {
		e := newImportEnv(t)
		failRescans(t, e)
		res, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = path.Dir(copperGrove) }))
		if err != nil {
			t.Fatal(err)
		}
		if got := res.(*ImportResult); got.Path != path.Dir(copperGrove) || e.accts.downloads != 1 {
			t.Fatalf("result %s, downloads %d", mustJSON(t, got), e.accts.downloads)
		}
		e.tmpEmpty(t)
	})
}

// A marker's file read back with its time moved within markerSlack (a share
// keeping whole seconds) is still the file, and one the Bridge can't read for
// a moment keeps the marker for the retry after.
func TestImportMarkerSurvivesTheShare(t *testing.T) {
	t.Run("time rounded", func(t *testing.T) {
		e := newImportEnv(t)
		failRescans(t, e)
		p := filepath.Join(e.root, filepath.FromSlash(copperGrove))
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		later := st.ModTime().Add(time.Second)
		if err := os.Chtimes(p, later, later); err != nil {
			t.Fatal(err)
		}
		if _, err := e.run(context.Background(), e.params(nil)); err != nil || e.accts.downloads != 1 {
			t.Fatalf("err %v, downloads %d", err, e.accts.downloads)
		}
	})
	t.Run("unreadable for a moment", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs unix permissions, not root")
		}
		e := newImportEnv(t)
		failRescans(t, e)
		author := filepath.Join(e.root, "Mara Quill")
		if err := os.Chmod(author, 0); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(author, 0o755) }()
		_, err := e.run(context.Background(), e.params(nil))
		var f *Failure
		if err == nil || errors.As(err, &f) {
			t.Fatalf("err = %v, want a plain error (tried again)", err)
		}
		if _, err := os.Stat(filepath.Join(e.tmp, "acct0001-B0FAKE0002"+placedSuffix)); err != nil {
			t.Fatalf("the marker went: %v", err)
		}
		if err := os.Chmod(author, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := e.run(context.Background(), e.params(nil)); err != nil || e.accts.downloads != 1 {
			t.Fatalf("err %v, downloads %d", err, e.accts.downloads)
		}
		e.tmpEmpty(t)
	})
}

// A destination the Bridge can't follow in the library (a file where a folder
// goes, a link out of the mapping) is not_writable before Audible is asked
// for anything.
func TestImportRefusesAnUnreachableDestination(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, root string){
		"a file where a folder goes": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "Loose"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"a link out of the mapping": func(t *testing.T, root string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "Loose")); err != nil {
				t.Skip(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newImportEnv(t)
			setup(t, e.root)
			_, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = "Loose/Book" }))
			var f *Failure
			if !errors.As(err, &f) || f.Code != CodeNotWritable {
				t.Fatalf("err = %v, want not_writable", err)
			}
			if e.accts.licenses != 0 || e.accts.downloads != 0 {
				t.Fatalf("licenses %d, downloads %d", e.accts.licenses, e.accts.downloads)
			}
			e.tmpEmpty(t)
		})
	}
}

// A title Audible offers only as a DRM-free MP3 fails with its own code
// (drm_free, bridge#46: the console says why), and leaves nothing.
func TestImportDRMFreeTitle(t *testing.T) {
	e := newImportEnv(t)
	e.accts.licErr = []error{fmt.Errorf("%w: B0FAKE0002", audible.ErrDRMFree)}
	res, err := e.run(context.Background(), e.params(nil))
	var f *Failure
	if !errors.As(err, &f) || f.Code != CodeDRMFree || res != any(f) || !strings.Contains(err.Error(), "DRM-free MP3") {
		t.Fatalf("res %#v, err %v", res, err)
	}
	if e.accts.downloads != 0 || len(e.libraryFiles()) != 0 {
		t.Fatalf("downloads %d, library %v", e.accts.downloads, e.libraryFiles())
	}
	e.tmpEmpty(t)
}

// The destination follows the library's own spelling of the folders that
// exist (the path the server indexes): on a case-insensitive disk an existing
// "mara quill" takes the book; on a case-sensitive one "Mara Quill" is new.
func TestImportFollowsTheLibrarysSpelling(t *testing.T) {
	e := newImportEnv(t)
	if err := os.MkdirAll(filepath.Join(e.root, "mara quill"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(e.root, "MARA QUILL"))
	want := copperGrove
	if statErr == nil { // case-insensitive
		want = "mara quill" + strings.TrimPrefix(copperGrove, "Mara Quill")
	}
	res, err := e.run(context.Background(), e.params(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.(*ImportResult); got.Files[0].Path != want || !slices.Equal(e.srv.rescanned, []string{want}) {
		t.Fatalf("result %s, rescanned %v; want %s", mustJSON(t, got), e.srv.rescanned, want)
	}
	if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(want))); err != nil {
		t.Fatal(err)
	}
}
