package audiblejob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/importjob"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/placement"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// ImportKind is the job kind that backs one Audible title up into the
// account's books library (BRIDGE-PLAN.md Phase 4b).
const ImportKind = "audible.import"

// The steps an import reports (progress.step), in order.
const (
	StepLicense  = "license"
	StepDownload = "download"
	StepDecrypt  = "decrypt"
	StepPlace    = "place"
	StepRescan   = "rescan"
	StepEnrich   = "enrich"
)

// The codes a failed import reports (its result {code, message}); any other
// failure has none, only its error.
const (
	CodeNotReleased    = "not_released"           // a preorder, or Audible says it isn't out
	CodeNoRights       = "no_rights"              // not owned, no download rights, a show or periodical
	CodeRefused        = "audible_refused"        // Audible refused the sign-in, the device (throttled), or the license for another reason
	CodeNotWritable    = transfer.CodeNotWritable // not the account's library, not a checked writable mapping, or no valid path there
	CodeExists         = "exists"                 // a different file is at the destination
	CodeDiskFull       = "disk_full"              // the Bridge's temp space or the library is short of room
	CodeURLExpired     = "offline_url_expired"    // the download URL expired again after one new license
	CodeUnknownAccount = "unknown_account"        // not this Bridge's account, or mapped on another server
	// CodeDRMFree: Audible offers the title only as a DRM-free MP3, which the
	// Bridge doesn't back up yet (bridge#46).
	CodeDRMFree = "drm_free"
	// CodeSignIn: a poll Audible refused the sign-in for now (the next poll
	// tries again); an import's sign-in refusal stays audible_refused.
	CodeSignIn = "audible_sign_in"
)

// tmpHeadroom is the free-space factor for the Bridge's temp folder: it holds
// the AAXC and the decrypted copy at once (2 x, plus a margin). The library
// wants transfer's own (transfer.DefaultHeadroom).
const tmpHeadroom = 2.2

// PartialMaxAge is how long the start-up sweep keeps a partial download (a
// job that never resumed: cancelled while the Bridge was down, or lost).
const PartialMaxAge = transfer.StagingMaxAge

// Only a successful decrypt is renamed to this suffix. Older .m4b files can be incomplete.
const readySuffix = ".ready.m4b"

// downloadAttempts bounds one license's download attempts in a row that add
// nothing to the partial (a dropped connection resumes from it each time; an
// attempt that added to it starts the count again).
const downloadAttempts = 3

// mappingPoll is how often a job waiting on its library's mapping (a check not
// made yet, or a mapping to come back) looks again.
const mappingPoll = transfer.MappingPoll

// Failure is a failed import's result and error: a code from the list above
// and a sentence for the console, and for CodeExists what is in the way
// (Conflict: link.ConflictFile, ConflictBook or ConflictAudio, the plan's
// words, which the console reads rather than the sentence; bridge#47). It
// never holds a key, an iv or a URL, nor an Amazon account identifier (fail
// redacts the errors it quotes; Amazon's other text, an unavailable reason,
// is redacted where it is quoted).
type Failure struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Conflict string `json:"conflict,omitempty"`
}

func (f *Failure) Error() string { return f.Message }

// fail is a coded failure. An error among args (where Amazon's answer reaches
// a message: a license's denial, an HTTP error) passes audible.RedactedError;
// the rest (library paths, book titles, names, counts) are the Bridge's own
// and stay whole, since the redactor's shapes would cut words out of a title
// ("Basic Economics" reads as a Basic credential).
func fail(code, format string, args ...any) *Failure {
	quoted := make([]any, len(args))
	for i, a := range args {
		if err, ok := a.(error); ok {
			a = audible.RedactedError(err)
		}
		quoted[i] = a
	}
	return &Failure{Code: code, Message: fmt.Sprintf(format, quoted...)}
}

// exists is a CodeExists failure: conflict is what is in the way.
func exists(conflict, format string, args ...any) *Failure {
	f := fail(CodeExists, format, args...)
	f.Conflict = conflict
	return f
}

// ThrottledText is an import's message (code audible_refused) when Audible
// denies the license to the account's device (audible.ErrThrottled), as it
// does a device registered with an older profile. Signing in again removes the
// old device itself (audibleacct.Finish), and the new one carries the same
// name, so the message never asks for one to be removed at Amazon by hand.
const ThrottledText = "Audible refused this device. Sign in to Audible again on the Bridge (Accounts): " +
	"that registers a new AudioSilo device Audible allows downloads to, and removes this one."

// throttledDevice is the failure when Audible denies the account's device its
// licenses (ThrottledText).
func throttledDevice() *Failure { return fail(CodeRefused, "%s", ThrottledText) }

// refusedSignIn is the failure when Audible refuses the account's sign-in.
func refusedSignIn() *Failure {
	return fail(CodeRefused, "Audible refused the account's sign-in: sign in again on the Bridge's Accounts tab.")
}

// ImportParams are audible.import's params, built by the server: the account,
// the title, the account's books library, and the console's destination
// override (dest_path) and the account's path template ("" for none: Auto).
// Unknown fields are ignored (the server may add some).
type ImportParams struct {
	Account   string `json:"account"`
	ASIN      string `json:"asin"`
	LibraryID int64  `json:"library_id"`
	DestPath  string `json:"dest_path"`
	Template  string `json:"template"`
}

// maxTemplate bounds a path template and maxDestPath a destination, in bytes:
// the server's own bounds (bridge.MaxAudibleTemplate, MaxAudibleDestPath), so a
// template the server would refuse is never taken from a job (bridge#47).
const (
	maxTemplate = 500
	maxDestPath = 1024
)

// ParseImportParams reads and checks the params.
func ParseImportParams(raw json.RawMessage) (ImportParams, error) {
	var p ImportParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return ImportParams{}, fmt.Errorf("bad params: %w", err)
	}
	p.ASIN = strings.ToUpper(strings.TrimSpace(p.ASIN))
	switch {
	case !audibleacct.IDPattern.MatchString(p.Account):
		return ImportParams{}, errors.New("bad params: account must be an Audible account id")
	case !audibleacct.ASINPattern.MatchString(p.ASIN):
		return ImportParams{}, errors.New("bad params: asin must be an ASIN")
	case p.LibraryID <= 0:
		return ImportParams{}, errors.New("bad params: library_id is required")
	case len(p.Template) > maxTemplate || len(p.DestPath) > maxDestPath:
		return ImportParams{}, errors.New("bad params: the template or dest_path is too long")
	}
	return p, nil
}

// ImportResult is a done import: where the book is (the server's book path),
// the files placed and their bytes, and whether the book carries the title's
// ASIN now.
type ImportResult struct {
	LibraryID int64        `json:"library_id"`
	Path      string       `json:"path"`
	Files     []ImportFile `json:"files"`
	Bytes     int64        `json:"bytes"`
	Enriched  bool         `json:"enriched"`
}

// ImportFile is one placed file, library-relative.
type ImportFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ImportAccounts is the part of the account manager an import uses
// (*audibleacct.Manager): the account, one title's metadata, its license and
// its download, with the account's sign-in.
type ImportAccounts interface {
	Account(ctx context.Context, id string) (store.AudibleAccount, error)
	// SettledAccount is Account read past a sign-in in flight when the
	// account reads refused (audibleacct.Manager.SettledAccount).
	SettledAccount(ctx context.Context, id string) (store.AudibleAccount, error)
	// AwaitedAccount is Account read past a sign-in, mapping or removal in
	// flight (audibleacct.Manager.AwaitedAccount).
	AwaitedAccount(ctx context.Context, id string) (store.AudibleAccount, error)
	Title(ctx context.Context, id, asin string) (source.Record, error)
	License(ctx context.Context, id, asin string) (audible.License, error)
	Download(ctx context.Context, id, url, dest string, opts audible.DownloadOptions) error
}

// ImportServer is the part of the server link an import calls (*link.Client).
type ImportServer interface {
	RejectedAudibleTitles(ctx context.Context, account string) (map[string]bool, error)
	Book(ctx context.Context, libraryID int64, path string) (*link.Book, error)
	LibraryBooks(ctx context.Context, libraryID int64, after string, limit int) (*link.LibraryBooks, error)
	Rescan(ctx context.Context, libraryID int64, path string) (string, error)
	Enrich(ctx context.Context, libraryID int64, path, asin string) error
}

// Libraries is the mapping registry an import checks (*mapping.Registry).
type Libraries = transfer.Libraries

// ImportOptions wire the import handler.
type ImportOptions struct {
	Accounts ImportAccounts
	Server   func() ImportServer
	ServerID func() string
	// Pairing is the Bridge's pairing now (jobs.Options.Pairing's): an import
	// writes to a library only while it is the one its job was claimed under
	// (mapped). nil is always the zero Pairing.
	Pairing   func() jobs.Pairing
	Libraries Libraries
	Mount     mapping.MountFunc
	// TmpDir is <data>/audible/tmp: the partial downloads and decrypted copies.
	TmpDir string
	// FFmpeg is the ffmpeg to decrypt with ("" is ffmpeg on PATH).
	FFmpeg func() string
	Log    *slog.Logger

	// Seams (tests): the decrypt step, the free-space reader, and the wait
	// between attempts and between looks at the library's mapping.
	Decrypt func(ctx context.Context, ffmpeg, in, out string, lic audible.License) error
	Free    func(dir string) (int64, error)
	Wait    func(ctx context.Context, d time.Duration) error
	// onProgress sees every progress the job reports (tests).
	onProgress func(jobs.Progress)
}

// Importer runs audible.import, one at a time per account (two accounts
// download side by side when the lane has room).
type Importer struct {
	o     ImportOptions
	local *transfer.Local

	mu      sync.Mutex
	gates   map[string]*accountGate
	listing *bookSnapshot
}

// accountGate is one account's gate (gate): its token, and the reporter of
// the import holding it (nil while Abandon holds it, or no import does).
type accountGate struct {
	token  chan struct{}
	holder atomic.Pointer[jobs.Reporter]
}

// held records rep as the gate's holder, the token just taken, and returns
// the release.
func (g *accountGate) held(rep *jobs.Reporter) func() {
	g.holder.Store(rep)
	return func() {
		g.holder.Store(nil)
		<-g.token
	}
}

// NewImport returns the import handler.
func NewImport(o ImportOptions) *Importer {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.ServerID == nil {
		o.ServerID = func() string { return "" }
	}
	if o.Pairing == nil {
		o.Pairing = func() jobs.Pairing { return jobs.Pairing{} }
	}
	if o.FFmpeg == nil {
		o.FFmpeg = func() string { return "" }
	}
	if o.Decrypt == nil {
		o.Decrypt = audible.Decrypt
	}
	if o.Free == nil {
		o.Free = fsutil.FreeBytes
	}
	if o.Wait == nil {
		o.Wait = link.Wait
	}
	return &Importer{o: o, local: transfer.NewLocal(), gates: map[string]*accountGate{}}
}

// Kind is audible.import.
func (h *Importer) Kind() string { return ImportKind }

// ServerQueued: the server queues and schedules imports again itself.
func (h *Importer) ServerQueued() bool { return true }

// Lane is the download lane.
func (h *Importer) Lane() string { return jobs.LaneDownload }

// tmpBase is the name every temp file of the account's title starts with.
func (h *Importer) tmpBase(account, asin string) string {
	return transfer.PrivateDir(h.o.TmpDir).Base(account, asin)
}

// partPaths are the job's temp files: the partial AAXC (kept across a restart,
// with its state file) and the decrypted copy.
func (h *Importer) partPaths(account, asin string) (part, decrypted string) {
	base := h.tmpBase(account, asin)
	return base + ".aaxc.part", base + readySuffix
}

// Abandon removes the temp files of a job the runner ended without running it
// (cancelled before it resumed, or dropped: another server's, or its pairing
// ended). A job whose claim was lost keeps its partial: the server hands the
// job out again (a lease that lapsed while the Bridge was down), and the next
// claim resumes it; one that never comes back is the start-up sweep's
// (PartialMaxAge). The title's temp files are an import's only while it holds
// the account's gate (run): while another import of the account holds it (a
// later claim of the same title among them), they are left to it, and to the
// sweep. Abandon never waits for the gate.
func (h *Importer) Abandon(ctx context.Context, job jobs.Job) {
	if jobs.Lost(ctx) {
		return
	}
	p, err := ParseImportParams(job.Params)
	if err != nil {
		return
	}
	release, ok := h.tryGate(p.Account, nil)
	if !ok {
		return
	}
	defer release()
	part, decrypted := h.partPaths(p.Account, p.ASIN)
	audible.RemovePartial(part)
	removeStaged(decrypted)
}

// Run imports the title: license, download, decrypt, place, rescan, enrich.
func (h *Importer) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	p, err := ParseImportParams(job.Params)
	if err != nil {
		return nil, err
	}
	if job.LibraryID != nil && *job.LibraryID != p.LibraryID {
		return nil, errors.New("bad params: library_id isn't the job's library")
	}
	res, err := h.run(ctx, job, p, rep)
	var f *Failure
	if errors.As(err, &f) {
		return f, f
	}
	if err != nil {
		// Belt and braces: the audible client redacts Amazon's text where it
		// becomes an error, and the job's error passes the same redactor.
		return nil, audible.RedactedError(err)
	}
	return res, nil
}

// importable checks an account read of p's import (acct and its error): the
// account is this Bridge's, mapped on serverID (the job's server), with p's
// library its books library.
func (h *Importer) importable(acct store.AudibleAccount, err error, serverID string, p ImportParams) error {
	switch {
	case errors.Is(err, audibleacct.ErrNotFound):
		return fail(CodeUnknownAccount, "This Bridge has no Audible account %s: it was removed here, or signed in on another Bridge.", p.Account)
	case err != nil:
		return err
	case acct.OnOtherServer(serverID):
		return fail(CodeUnknownAccount, "The Audible account is mapped on another server: map it again on the Bridge's Accounts tab.")
	case acct.LibraryID != p.LibraryID:
		return fail(CodeNotWritable, "Library %d isn't the Audible account's books library: map the account again or queue the import again.", p.LibraryID)
	}
	return nil
}

func (h *Importer) run(ctx context.Context, job jobs.Job, p ImportParams, rep *jobs.Reporter) (res *ImportResult, err error) {
	// One import of the account at a time: while it holds the gate, the
	// title's temp files are this job's alone (an older attempt of the same job
	// still queued here never touches them).
	release, err := h.gate(ctx, p.Account, rep)
	if err != nil {
		return nil, err
	}
	defer release()
	part, decrypted := h.partPaths(p.Account, p.ASIN)
	defer func() {
		// A stop the next start resumes, and a lost claim the server hands out
		// again, keep the temp files; an import whose pairing ended
		// (errImportPairingGone, or however it ended once the Bridge is
		// paired otherwise) resumes under no claim, so it never does.
		pairingGone := errors.Is(err, errImportPairingGone) || !job.Under(h.o.Pairing())
		if err != nil && ctx.Err() != nil && (jobs.Resumes(ctx) || jobs.Lost(ctx)) && !pairingGone {
			return
		}
		// A failure placing the book can be retried with the same decrypt.
		// Cancellation, a dropped pairing or a definitive refusal ends its use.
		// A 404 for the job is no refusal: a proxy in front of a restarting
		// server answers one too, and a title queued again (by this account)
		// reuses the decrypt. The start-up sweep bounds what a removed job leaves.
		if err == nil || ctx.Err() != nil || bookRefused(err) || pairingGone {
			removeStaged(decrypted)
		}
		audible.RemovePartial(part)
	}()

	serverID := job.KeyServer(h.o.ServerID()) // the job's, as the poll's
	acct, err := h.o.Accounts.Account(ctx, p.Account)
	// As the poll's read: a sign-in or a mapping the server recorded, and
	// queued this import for, may still be storing itself here (no account
	// yet, another server's or another library's). Read again past it before
	// failing for that; an account that passes never waits.
	if audibleacct.NotHere(acct, err, serverID) || (err == nil && acct.LibraryID != p.LibraryID) {
		acct, err = h.o.Accounts.AwaitedAccount(ctx, p.Account)
	}
	if err := h.importable(acct, err, serverID, p); err != nil {
		return nil, err
	}
	library := importLibrary{server: serverID, pairing: job.Pairing(), id: p.LibraryID}
	var srv ImportServer
	if h.o.Server != nil {
		srv = jobs.Link(job, h.o.Server())
	}
	if srv == nil {
		return nil, errors.New("the Bridge isn't paired with a server")
	}

	stage := readStaged(decrypted, library, p.ASIN)
	lib, mount, err := h.mapped(ctx, job, p.LibraryID, rep, StepLicense, !job.Resumed && stage == nil)
	if err != nil {
		return nil, err
	}
	if err := transfer.PrivateDir(h.o.TmpDir).Create(); err != nil {
		return nil, fmt.Errorf("the Bridge's temp folder: %w", err)
	}

	// An earlier run installed the book and then failed (the rescan): its
	// marker says what it placed, and while the library holds exactly that the
	// job goes on from the rescan instead of failing exists: at once for the
	// same request (dest_path and template), else once this request's
	// destination turns out to be the same file.
	marker := h.markerPath(acct.ID, p.ASIN)
	earlier, err := readPlaced(marker, lib.ID, mount)
	if err != nil {
		return nil, err
	}
	if earlier != nil && earlier.request(p) {
		return h.finish(ctx, rep, srv, acct.ID, p.ASIN, library, earlier.installed(), marker)
	}

	// A process can die between publishing the staged bytes and writing its
	// placement marker. Only adopt that destination if it is the same book.
	if stage != nil && stage.LibraryID == lib.ID && stage.request(p) {
		same, err := transfer.Matches(ctx, mount, stage.Path, decrypted)
		if err != nil {
			return nil, err
		}
		if same {
			h.recordPlacement(marker, p, mount, stage.Path, stage.Joins, decrypted)
			return h.finish(ctx, rep, srv, acct.ID, p.ASIN, library,
				stage.installed(), marker)
		}
	}
	// A refused account fails here, before asking Audible anything: each queued
	// import would only be refused in turn (a device denied licenses, for one,
	// stays denied) until the owner signs in again, which ends the refusal.
	// Read again with SettledAccount: the job may have waited for the
	// library's mapping, and a sign-in again meanwhile, or one still storing
	// itself, ended the refusal the first read saw. The read again is checked
	// as the first was: what it waited for may have removed the account or
	// mapped it elsewhere. A staged decrypt asks Audible nothing more, so a
	// refusal since never throws it away.
	if stage == nil && acct.RefusedAt != "" {
		acct, err = h.o.Accounts.SettledAccount(ctx, acct.ID)
		if err := h.importable(acct, err, serverID, p); err != nil {
			return nil, err
		}
		if acct.RefusedAt != "" {
			if audibleacct.Throttled(acct) {
				return nil, throttledDevice()
			}
			return nil, refusedSignIn()
		}
	}

	var rec source.Record
	if stage != nil {
		rec = stage.Title
	} else {
		rec, err = h.readTitle(ctx, p, rep)
		if err != nil {
			return nil, err
		}
	}

	var books []link.LibraryBook
	checkPaths := strings.TrimSpace(p.DestPath) != ""
	if !checkPaths {
		books, checkPaths, err = h.booksFor(ctx, srv, library)
		if err != nil {
			return nil, fmt.Errorf("reading the library's books on the server: %w", err)
		}
	}
	rel, err := Destination(p, rec, books)
	if err != nil {
		return nil, err
	}
	if rel, err = h.spelled(mount, rel); err != nil {
		return nil, err
	}
	if earlier != nil {
		if earlier.Path == rel {
			return h.finish(ctx, rep, srv, acct.ID, p.ASIN, library, earlier.installed(), marker)
		}
		// Placed for another destination: that copy stays where it is, and
		// the title goes where this request asks.
		h.o.Log.Info("audible: an earlier copy of the title stays where an earlier request placed it",
			"account", acct.ID, "asin", p.ASIN, "library", lib.ID, "path", earlier.Path)
		_ = os.Remove(marker)
	}
	if checkPaths {
		books, err = booksAt(ctx, srv, lib.ID, mount, rel)
		if err != nil {
			return nil, fmt.Errorf("checking the destination on the server: %w", err)
		}
	}
	if err := h.vacant(mount, rel, books); err != nil {
		return nil, err
	}
	joins := joinedBooks(rec, books, rel)

	next := stagedBook{Server: serverID, Pairing: library.pairing, Title: rec,
		placedMarker: placedMarker{LibraryID: lib.ID, placedRequest: requestOf(p), Path: rel, Joins: joins}}
	if stage == nil {
		if err := h.prepare(ctx, p, part, decrypted, mount, rel, rep, next); err != nil {
			if fsutil.IsNoSpace(err) {
				return nil, fail(CodeDiskFull, "The Bridge ran out of space staging the book in its data folder.")
			}
			return nil, err
		}
	} else {
		next.Size, next.ModTime = stage.Size, stage.ModTime
		if err := next.save(decrypted); err != nil {
			return nil, err
		}
	}
	audible.RemovePartial(part)

	h.report(rep, jobs.Progress{Step: StepPlace, Code: jobs.SayCopyingBook})
	// Checked again before the copy: the mapping may have changed during the
	// download. Not writable now, the job parks with its decrypted copy and
	// waits for it to come back rather than throwing the download away.
	if _, mount, err = h.mapped(ctx, job, lib.ID, rep, StepPlace, false); err != nil {
		return nil, err
	}
	placed, err := h.local.Place(ctx, transfer.PlaceRequest{Root: mount, RelPath: rel, SrcPath: decrypted, LinkSource: true,
		BeforeInstall: rep.ConfirmLease,
		Progress: func(copied, total int64) {
			h.report(rep, jobs.Progress{Step: StepPlace, Code: jobs.SayCopyingBook, Done: int(copied), Total: int(total)})
		}})
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, transfer.ErrExists):
		return nil, exists(link.ConflictFile, "A different file is already at %s: nothing was overwritten. Choose another destination, or move that file.", rel)
	case errors.Is(err, transfer.ErrDiskFull):
		return nil, fail(CodeDiskFull, "The library is short of space for the book (%s).", err)
	case errors.Is(err, transfer.ErrInvalidPath), errors.Is(err, fs.ErrPermission), fsutil.IsReadOnly(err):
		return nil, fail(CodeNotWritable, "The Bridge can't write %s in the library: %v", rel, err)
	case err != nil:
		return nil, fmt.Errorf("copying the book into the library: %w", err)
	}

	h.recordPlacement(marker, p, mount, placed.RelPath, joins, decrypted)
	return h.finish(ctx, rep, srv, acct.ID, p.ASIN, library,
		installed{rel: placed.RelPath, size: placed.Size, already: placed.AlreadyPresent, joins: joins}, marker)
}

// bookRefused identifies failures that say this account or title is no longer
// eligible. A failure reading a library or writing the destination can be retried.
func bookRefused(err error) bool {
	var f *Failure
	if errors.As(err, &f) {
		return f.Code == CodeUnknownAccount || f.Code == CodeNoRights
	}
	var api *link.APIError
	if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 && !api.Transient() {
		return api.Code == CodeUnknownAccount || api.Code == "unknown_title" || api.Code == CodeNoRights
	}
	return false
}

// Once the marker records the installed book, it takes over recovery from the decrypt.
func (h *Importer) recordPlacement(marker string, p ImportParams, mount, rel string, joins map[string]bool, decrypted string) {
	if err := writePlaced(marker, requestOf(p), p.LibraryID, mount, rel, joins); err != nil {
		h.o.Log.Warn("audible: recording an imported book's placement failed", "account", p.Account, "asin", p.ASIN, "err", err)
		return
	}
	removeStaged(decrypted)
}

// A ready file and its metadata were written by the same completed decrypt.
// The marker's Size and ModTime are the staged file's here, and its request,
// library, path and joins are the placement the stage was planned for.
type stagedBook struct {
	placedMarker
	Server  string        `json:"server"`
	Pairing string        `json:"pairing,omitempty"`
	Title   source.Record `json:"title"`
}

// stagedMeta is the metadata file beside a ready decrypt.
func stagedMeta(file string) string { return file + ".json" }

// removeStaged removes a ready decrypt and its metadata.
func removeStaged(file string) {
	_ = os.Remove(file)
	_ = os.Remove(stagedMeta(file))
}

func (m stagedBook) save(file string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(stagedMeta(file), b, 0o600)
}

func readStaged(file string, library importLibrary, asin string) *stagedBook {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(stagedMeta(file)) //nolint:gosec // named within the private import folder
	var m stagedBook
	if err != nil || json.Unmarshal(b, &m) != nil || m.Server != library.server || m.Pairing != library.pairing ||
		m.Title.ASIN != asin || m.Size <= 0 || m.Size != info.Size() || m.ModTime != info.ModTime().UnixNano() {
		return nil
	}
	return &m
}

func (h *Importer) readTitle(ctx context.Context, p ImportParams, rep *jobs.Reporter) (source.Record, error) {
	h.report(rep, jobs.Progress{Step: StepLicense, Code: jobs.SayReadingAudibleTitle})
	rec, err := h.o.Accounts.Title(ctx, p.Account, p.ASIN)
	switch {
	case errors.Is(err, audibleacct.ErrRefused):
		return source.Record{}, refusedSignIn()
	case errors.Is(err, audible.ErrNotInLibrary):
		return source.Record{}, fail(CodeNoRights, "The title isn't in the Audible account's library.")
	case err != nil:
		return source.Record{}, fmt.Errorf("reading the title from Audible: %w", err)
	case rec.Preorder:
		return source.Record{}, fail(CodeNotReleased, "The title isn't released yet (%s).", audible.RedactAccountIDs(rec.UnavailableReason))
	case strings.TrimSpace(rec.UnavailableReason) != "":
		return source.Record{}, fail(CodeNoRights, "Audible won't let the account download the title: %s.", audible.RedactAccountIDs(rec.UnavailableReason))
	}
	rec.ASIN = p.ASIN
	return rec, nil
}

// prepare writes the title and completed audio before discarding the encrypted download.
func (h *Importer) prepare(ctx context.Context, p ImportParams, part, decrypted, mount, rel string, rep *jobs.Reporter, stage stagedBook) error {
	// A prior incomplete or invalid pair must never be vouched for by new metadata.
	if err := os.Remove(decrypted); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lic, err := h.license(ctx, p.Account, p.ASIN)
	if err != nil {
		return err
	}
	h.report(rep, jobs.Progress{Step: StepDownload, Code: jobs.SayDownloadingAudible})
	if lic, err = h.download(ctx, p.Account, p.ASIN, lic, part, mount, rel, rep); err != nil {
		return err
	}

	h.report(rep, jobs.Progress{Step: StepDecrypt, Code: jobs.SayRemovingDRM})
	// A stopped ffmpeg can leave a valid-looking but incomplete MP4. Keep its
	// output separate until it has closed successfully.
	work := decrypted + ".partial"
	defer func() { _ = os.Remove(work) }()
	// Use the final library mode (subject to the umask) inside the private temp folder.
	f, err := os.OpenFile(work, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644) //nolint:gosec // G302: final library mode inside a private 0700 folder
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	err = h.o.Decrypt(ctx, h.o.FFmpeg(), part, work, lic)
	switch {
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil && ranOutOfRoom(err):
		return fail(CodeDiskFull, "The Bridge ran out of space removing the DRM: free some space in its data folder.")
	case err != nil:
		return fmt.Errorf("removing the DRM: %w", err)
	}
	// Stat once synced: readStaged compares this size and time exactly.
	if err := f.Sync(); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("the decrypted book is empty")
	}
	if err := f.Close(); err != nil {
		return err
	}
	stage.Size, stage.ModTime = info.Size(), info.ModTime().UnixNano()
	if err := stage.save(decrypted); err != nil {
		return err
	}
	return os.Rename(work, decrypted)
}

// isDiscFolder mirrors the server's metadata.IsDiscFolder for the rescan join.
func isDiscFolder(name string) bool {
	name = discSeparators.Replace(strings.ToLower(strings.TrimSpace(name)))
	for _, prefix := range []string{"cd", "disc", "disk"} {
		if number, ok := strings.CutPrefix(name, prefix); ok && strings.Trim(number, "0123456789") == "" {
			return true
		}
	}
	return false
}

var discSeparators = strings.NewReplacer(" ", "", "-", "", ".", "", "_", "")

// discParent is the folder above rel's disc folder (rel in "<parent>/CD2"),
// where a rescan can join the disc with its siblings; ok is false when rel's
// folder isn't a disc folder.
func discParent(rel string) (parent string, ok bool) {
	dir := path.Dir(rel)
	return path.Dir(dir), isDiscFolder(path.Base(dir))
}

// joinedBooks is, for each book of the library the file at rel could join (the
// paths the server's rescan can answer for a book it already had: the file's
// own, its folder's, or a disc set's above it), whether that book may take the
// title's ASIN: only under the poll's rule, the matcher over the library
// picking that book for the title, the title not spanning books the book
// doesn't (importjob.SpanMismatch), so an omnibus never names a single volume
// and a title never names another book, and the book not one the library
// holds twice (importjob.Twins: the title's words can't tell the copies
// apart, bridge#64). The book the rescan answers decides:
// one not listed here is new, the title itself (a pack placed as a pack
// included), and takes the ASIN, unless it is the disc folder's parent
// (finish: the server joins a disc there only beside another, indexed or
// not, so the join is never the title's own).
func joinedBooks(rec source.Record, books []link.LibraryBook, rel string) map[string]bool {
	joins := map[string]bool{}
	var twins map[string]bool
	var match link.LibraryBook
	matched, looked := false, false
	dir := path.Dir(rel)
	parent := path.Dir(dir)
	for _, b := range books {
		if b.Path == "." || (b.Path != rel && b.Path != dir && b.Path != parent) {
			continue
		}
		if !looked {
			match, matched = importjob.MatchInBooks(books, rec)
			twins = importjob.Twins(books)
			looked = true
		}
		joins[b.Path] = matched && match.Path == b.Path && !importjob.SpanMismatch(rec, b) && !twins[b.Path]
	}
	return joins
}

// installed is the file an install left in the library: where, its size,
// whether it was there already (an earlier run's), and the books it could join
// that may take the title's ASIN (joinedBooks).
type installed struct {
	rel     string
	size    int64
	already bool
	joins   map[string]bool
}

// finish is the steps after the install: the rescan, the ASIN (unless the
// book it answers is one the file joined that may not take it, a disc
// folder's parent not listed, or a joined book while the title is marked Not
// this book) and the result. The placement marker goes once the result is
// made.
func (h *Importer) finish(ctx context.Context, rep *jobs.Reporter, srv ImportServer, account, asin string, library importLibrary,
	in installed, marker string) (*ImportResult, error) {
	libraryID := library.id
	rel, size := in.rel, in.size
	h.report(rep, jobs.Progress{Step: StepRescan, Code: jobs.SayIndexingBook})
	bookPath, err := h.rescan(ctx, srv, libraryID, rel)
	if err != nil {
		return nil, fmt.Errorf("the book is in the library at %s, but the server couldn't index it: %w", rel, err)
	}

	h.recordBook(library, bookPath)
	enrich, joined := in.joins[bookPath]
	switch {
	case !joined:
		// A rescan joins a disc at its parent only beside another disc (one
		// indexed or not at placement): that book isn't the title's own.
		parent, disc := discParent(rel)
		enrich = !disc || bookPath != parent
	case enrich:
		// The admin may have marked the title since the placement (or a
		// previous attempt's marker) gave the joined book its verdict.
		rejected, err := srv.RejectedAudibleTitles(ctx, account)
		if errors.Is(err, link.ErrUnknownAccount) {
			return nil, fail(CodeUnknownAccount, "The book is in the library at %s, but the server has no such Audible account: map the account again on the Bridge's Accounts tab.", rel)
		}
		if err != nil {
			return nil, fmt.Errorf("checking the title before enriching a joined book: %w", err)
		}
		enrich = !rejected[asin]
	}
	enriched := false
	if enrich {
		h.report(rep, jobs.Progress{Step: StepEnrich, Code: jobs.SayAddingASIN})
		if enriched, err = h.enrichPlaced(ctx, rep, srv, account, libraryID, bookPath, asin); err != nil {
			return nil, err
		}
	} else {
		h.o.Log.Info("audible: the imported file joined a book that isn't the title: its ASIN isn't added",
			"account", account, "asin", asin, "library", libraryID, "path", bookPath)
	}
	h.o.Log.Info("audible: imported", "account", account, "asin", asin, "library", libraryID, "path", bookPath,
		"bytes", size, "already_there", in.already, "enriched", enriched)
	_ = os.Remove(marker)
	return &ImportResult{LibraryID: libraryID, Path: bookPath, Files: []ImportFile{{Path: rel, Size: size}},
		Bytes: size, Enriched: enriched}, nil
}

// enrichPlaced gives the imported book its title's ASIN, waiting out the
// server's 429s as the poll's enrichment does (enrichLimit); a 429 it gives up
// on, an ASIN set meanwhile or a moved book is no failure, only no ASIN.
func (h *Importer) enrichPlaced(ctx context.Context, rep *jobs.Reporter, srv ImportServer, account string, libraryID int64,
	bookPath, asin string) (bool, error) {
	var waitErr error
	err := enrichLimit.Do(ctx, func(ctx context.Context, wait time.Duration) error {
		h.report(rep, jobs.Progress{Step: StepEnrich, Code: jobs.SayServerSlowDown})
		waitErr = h.o.Wait(ctx, wait)
		return waitErr
	}, func(ctx context.Context) error {
		return srv.Enrich(ctx, libraryID, bookPath, asin)
	})
	_, limited := link.RateLimited(err)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, link.ErrASINSet), errors.Is(err, link.ErrNoBook):
	case ctx.Err() != nil:
		return false, ctx.Err()
	case waitErr != nil:
		return false, waitErr
	case limited:
		h.o.Log.Info("audible: the server kept refusing an imported book's ASIN (429): the book is placed without it",
			"account", account, "library", libraryID, "path", bookPath)
	default:
		h.o.Log.Warn("audible: adding an imported book's ASIN failed", "account", account, "library", libraryID, "err", err)
	}
	return false, nil
}

// placedRequest is the placement a job asked for (its trimmed dest_path and
// template; Auto when both are empty).
type placedRequest struct {
	DestPath string `json:"dest_path,omitempty"`
	Template string `json:"template,omitempty"`
}

// requestOf is p's placement request.
func requestOf(p ImportParams) placedRequest {
	return placedRequest{DestPath: strings.TrimSpace(p.DestPath), Template: strings.TrimSpace(p.Template)}
}

// placedMarker is what an install recorded: the request it placed for, the
// file it placed, with the size and modification time it had, so a retry
// knows the library still holds exactly that file (and nothing an
// administrator put there since), and which books the file could join may take
// the title's ASIN (joinedBooks).
type placedMarker struct {
	LibraryID int64 `json:"library_id"`
	placedRequest
	Path    string          `json:"path"`
	Size    int64           `json:"size"`
	ModTime int64           `json:"mtime_ns"`
	Joins   map[string]bool `json:"joins,omitempty"`
}

// request reports whether the marker was placed for p's request.
func (m *placedMarker) request(p ImportParams) bool { return m.placedRequest == requestOf(p) }

// installed is the marker's file, as an install left it.
func (m *placedMarker) installed() installed {
	return installed{rel: m.Path, size: m.Size, already: true, joins: m.Joins}
}

// markerSlack is how far the file's modification time may move from the one
// recorded and still be that file: a share, or a move between disks, may keep
// it only to the second (two on FAT).
const markerSlack = 2 * time.Second

// markerPath is the job's placement marker, beside its temp files.
func (h *Importer) markerPath(account, asin string) string {
	return h.tmpBase(account, asin) + placedSuffix
}

// placedSuffix names the placement markers (SweepTmp keeps the young ones).
const placedSuffix = ".placed.json"

// writePlaced records the file an install placed at rel for req.
func writePlaced(marker string, req placedRequest, libraryID int64, mount, rel string, joins map[string]bool) error {
	st, err := placedStat(mount, rel)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel)
	}
	b, err := json.Marshal(placedMarker{LibraryID: libraryID, placedRequest: req, Path: rel, Size: st.Size(),
		ModTime: st.ModTime().UnixNano(), Joins: joins})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(marker, b, 0o600)
}

// readPlaced answers the marker while the library at mount holds exactly the
// file it recorded (a regular file of the same size and about the same
// modification time), nil when there is none. A marker for another library,
// or whose file was changed or replaced since, goes. One whose file can't be
// read now (a share gone for a moment) stays, and the job fails to try again;
// one whose file isn't there stays too, for the job to place anew (the new
// install records its own).
func readPlaced(marker string, libraryID int64, mount string) (*placedMarker, error) {
	b, err := os.ReadFile(marker) //nolint:gosec // G304: the job's own marker in the Bridge's data
	if err != nil {
		return nil, nil //nolint:nilerr // no marker (or none readable) is none: the job places anew
	}
	var m placedMarker
	if json.Unmarshal(b, &m) != nil || m.LibraryID != libraryID {
		_ = os.Remove(marker)
		return nil, nil
	}
	if _, err := transfer.CleanRel(m.Path); err != nil {
		_ = os.Remove(marker)
		return nil, nil
	}
	st, err := placedStat(mount, m.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the book an earlier run placed at %s: %w", m.Path, err)
	case !st.Mode().IsRegular() || st.Size() != m.Size || st.ModTime().Sub(time.Unix(0, m.ModTime)).Abs() > markerSlack:
		_ = os.Remove(marker)
		return nil, nil
	}
	return &m, nil
}

// placedStat is the Lstat of a library file through the mount's root (never
// following a link out of it).
func placedStat(mount, rel string) (fs.FileInfo, error) {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Lstat(rel)
}

// report records the job's progress.
func (h *Importer) report(rep *jobs.Reporter, pr jobs.Progress) {
	rep.Progress(pr)
	if h.o.onProgress != nil {
		h.o.onProgress(pr)
	}
}

// gatePoll is how often an import waiting for its account's gate looks
// whether the import holding it gave its lane slot back.
const gatePoll = 2 * time.Second

// gate waits until no other import of the account runs: one download per
// account (Audible's limits are per account).
func (h *Importer) gate(ctx context.Context, account string, rep *jobs.Reporter) (func(), error) {
	if release, ok := h.tryGate(account, rep); ok {
		return release, nil
	}
	g := h.gateOf(account)
	h.report(rep, jobs.Progress{Step: StepLicense, Code: jobs.SayAccountBackupWait})
	tick := time.NewTicker(gatePoll)
	defer tick.Stop()
	for {
		// A holder without its lane slot (parked, or still reclaiming one)
		// needs one again to go on, so this import waits without one then.
		// While the holder keeps its own, this one keeps its slot too: a free
		// slot would have the lane claim the account's whole queue to wait here.
		if holder := g.holder.Load(); holder != nil && !holder.HoldsSlot() {
			rep.Yield()
		}
		select {
		case g.token <- struct{}{}:
			release := g.held(rep)
			if err := rep.Reclaim(ctx); err != nil {
				release()
				return nil, err
			}
			return release, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// gateOf is the account's gate (gate), made on first use.
func (h *Importer) gateOf(account string) *accountGate {
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.gates[account]
	if !ok {
		g = &accountGate{token: make(chan struct{}, 1)}
		h.gates[account] = g
	}
	return g
}

// tryGate takes the account's gate for rep (nil: Abandon) when no import of
// it holds it (false, without waiting, when one does).
func (h *Importer) tryGate(account string, rep *jobs.Reporter) (func(), bool) {
	g := h.gateOf(account)
	select {
	case g.token <- struct{}{}:
		return g.held(rep), true
	default:
		return nil, false
	}
}

// mapped waits for this job's library and download slot under its claim's pairing.
func (h *Importer) mapped(ctx context.Context, job jobs.Job, id int64, rep *jobs.Reporter, step string, strict bool) (link.Library, string, error) {
	w := transfer.MappingWait{
		Libraries: h.o.Libraries, Mount: h.o.Mount, Pairing: h.o.Pairing, PairingGone: errImportPairingGone,
		NotWritable: func(why string) error { return fail(CodeNotWritable, "%s", why) },
		Wait:        h.o.Wait, Report: h.report,
	}
	return w.Mapped(ctx, job, id, rep, step, strict)
}

// errImportPairingGone ends an import whose pairing ended under it (mapped).
var errImportPairingGone = errors.New("the Bridge's pairing that claimed this import has ended, so it writes nothing to the library")

// spelled is rel as the library spells the folders of it that already exist
// (transfer.OnDisk: the path the server indexes, on a case-insensitive disk).
// A path the Bridge can't follow there (a file where a folder goes, a link out
// of the mapping, a folder it may not read) is not_writable, before an hour's
// download.
func (h *Importer) spelled(mount, rel string) (string, error) {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return "", fail(CodeNotWritable, "The Bridge can't open the library at its mount: %v", err)
	}
	defer func() { _ = root.Close() }()
	return spelled(transfer.RootFS(root), rel)
}

// spelled is Importer.spelled over the library's files (a poll's plans read
// them through a view that lists each folder once).
func spelled(fsys transfer.LibraryFS, rel string) (string, error) {
	got, err := transfer.OnDisk(fsys, rel)
	if err != nil {
		return "", fail(CodeNotWritable, "The Bridge can't place the book at %s in the library: %v", rel, err)
	}
	return got, nil
}

// vacant fails early (before an hour's download) when something is already at
// the destination, or the Bridge can't tell. The copy checks again, and
// accepts the same bytes there. A backup is a book of its own, so a
// destination inside a book the library already has is exists too: a folder
// the server lists as a book (books), or one already holding audio (a book not
// indexed yet). Joining it would give an owned book a second copy of its audio
// (or another book's), and the server refuses a done import whose book isn't
// the title's own; a title the library already has is matched, not backed up.
func (h *Importer) vacant(mount, rel string, books []link.LibraryBook) error {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return fail(CodeNotWritable, "The Bridge can't open the library at its mount: %v", err)
	}
	defer func() { _ = root.Close() }()
	return vacant(transfer.RootFS(root), rel, bookPaths(books))
}

// bookPaths is each listed book's title by its path.
func bookPaths(books []link.LibraryBook) map[string]string {
	out := make(map[string]string, len(books))
	for _, b := range books {
		if _, ok := out[b.Path]; !ok {
			out[b.Path] = b.Title
		}
	}
	return out
}

// listedBook is the listed book (books: bookPaths) a destination rel is in or
// is: its folder's, or rel's own. A file at the library's root is in none.
func listedBook(rel string, books map[string]string) (at, title string, ok bool) {
	dir := path.Dir(rel)
	if dir == "." {
		return "", "", false // loose files at the library's root are books of their own
	}
	for _, at := range []string{dir, rel} {
		if title, ok := books[at]; ok {
			return dir, title, true
		}
	}
	return "", "", false
}

// vacant is Importer.vacant over the library's files: a CodeExists failure
// names what is in the way (conflictOf). The
// folder rel's would have been under the name rule before bridge#47 counts
// too (formerDirs): a title backed up as "Who Moved My Cheese?" is there
// already, and isn't downloaded again into "Who Moved My Cheese". So does a
// file at the library's root under its former name (formerFile; bridge#80: a
// "{Title}.m4b" template has no folder to find it by, and a file in a folder
// is that folder's audio already).
func vacant(fsys transfer.LibraryFS, rel string, books map[string]string) error {
	if err := vacantAt(fsys, rel, books); err != nil {
		return err
	}
	dir := path.Dir(rel)
	if dir == "." {
		if at := formerFile(fsys, dir, rel); at != "" {
			return vacantAt(fsys, at, books) // a file is there: ConflictFile
		}
		return nil
	}
	for _, dir := range formerDirs(fsys, dir) {
		// Only a book there counts; what can't be read there is no conflict.
		if err := vacantAt(fsys, path.Join(dir, path.Base(rel)), books); conflictOf(err) != "" {
			return err
		}
	}
	return nil
}

// conflictOf is what is in the way when err is a CodeExists failure
// (link.ConflictFile, ConflictBook or ConflictAudio), else "".
func conflictOf(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Conflict
	}
	return ""
}

// formerFile is the file in dir the name rule before bridge#47 gave what it
// names base now (formerName), or "" (also when dir can't be read: vacantAt
// says what is wrong with rel's own folders).
func formerFile(fsys transfer.LibraryFS, dir, base string) string {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if name := e.Name(); !e.IsDir() && formerName(name, base) {
			return path.Join(dir, name)
		}
	}
	return ""
}

// formerDirs are the folders in the library that dir would have been under
// the name rule before bridge#47 (placement.Sanitize kept ? * < > " | and
// control characters then, and never closed up a run of spaces): dir with one
// folder or more of its path under a name the rule kept then and gives that
// folder's name now. None for a folder the library doesn't have.
func formerDirs(fsys transfer.LibraryFS, dir string) []string {
	if dir == "." {
		return nil
	}
	type folder struct {
		path   string
		former bool
	}
	at := []folder{{path: "."}}
	for _, seg := range strings.Split(dir, "/") {
		var next []folder
		for _, f := range at {
			entries, err := fsys.ReadDir(f.path)
			if err != nil {
				continue // vacantAt says what is wrong with rel's own folders
			}
			for _, e := range entries {
				if name := e.Name(); e.IsDir() && (name == seg || formerName(name, seg)) {
					next = append(next, folder{path.Join(f.path, name), f.former || name != seg})
				}
			}
		}
		at = next
	}
	var out []string
	for _, f := range at {
		if f.former {
			out = append(out, f.path)
		}
	}
	return out
}

// formerName reports whether name is what the name rule before bridge#47 gave
// a text the rule now names seg: a name that rule could give (no separator or
// colon, no dot or space at either end) holding something the rule now drops,
// replaces or closes up, which placement.Sanitize turns into seg. A name
// Windows reserves (placement.Reserved: "Con", which Sanitize names "Con_"
// since bridge#80) is one too.
func formerName(name, seg string) bool {
	if placement.Reserved(name) {
		return placement.Sanitize(name) == seg
	}
	if len(name) < len(seg) || strings.ContainsAny(name, "/\\:\x00") || name != strings.Trim(strings.TrimSpace(name), " .") {
		return false
	}
	changed := strings.Contains(name, "  ") || strings.ContainsFunc(name, func(r rune) bool {
		return strings.ContainsRune("?*<>\"|", r) || unicode.IsControl(r) || (r != ' ' && unicode.IsSpace(r))
	})
	return changed && placement.Sanitize(name) == seg
}

// vacantAt is vacant for rel as it is spelled.
func vacantAt(fsys transfer.LibraryFS, rel string, books map[string]string) error {
	_, err := fsys.Lstat(rel)
	switch {
	case err == nil:
		return exists(link.ConflictFile, "A file is already at %s: nothing was overwritten. Choose another destination, or move that file.", rel)
	case !errors.Is(err, fs.ErrNotExist):
		return fail(CodeNotWritable, "The Bridge can't place the book at %s in the library: %v", rel, err)
	}
	if dir, title, ok := listedBook(rel, books); ok {
		return exists(link.ConflictBook, "%s is already the library's book %q: a backup goes in a folder of its own. Choose another destination.", dir, title)
	}
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	entries, err := fsys.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fail(CodeNotWritable, "The Bridge can't read the folder %s in the library: %v", dir, err)
	}
	for _, e := range entries {
		if ext := strings.ToLower(path.Ext(e.Name())); !e.IsDir() && (mp4Names[ext] || otherAudio[ext]) {
			return exists(link.ConflictAudio, "%s already holds audio (%s), a book of its own: a backup goes in a folder of its own. Choose another destination.", dir, e.Name())
		}
	}
	return nil
}

// license asks Audible for the title's license, saying why it was denied.
func (h *Importer) license(ctx context.Context, account, asin string) (audible.License, error) {
	lic, err := h.o.Accounts.License(ctx, account, asin)
	switch {
	case err == nil:
		return lic, nil
	case ctx.Err() != nil:
		return audible.License{}, ctx.Err()
	case errors.Is(err, audibleacct.ErrRefused):
		return audible.License{}, refusedSignIn()
	case errors.Is(err, audible.ErrThrottled):
		// The account's device, not the title: the message says how to fix it.
		return audible.License{}, throttledDevice()
	case errors.Is(err, audible.ErrNotReleased):
		return audible.License{}, fail(CodeNotReleased, "Audible says the title isn't released yet: %v", err)
	case errors.Is(err, audible.ErrNoRights):
		return audible.License{}, fail(CodeNoRights, "Audible won't license a download of the title to the account: %v", err)
	case errors.Is(err, audible.ErrDenied):
		return audible.License{}, fail(CodeRefused, "Audible refused a download license: %v", err)
	case errors.Is(err, audible.ErrDRMFree):
		return audible.License{}, fail(CodeDRMFree, "Audible offers the title only as a DRM-free MP3, which this version of the Bridge doesn't back up.")
	}
	return audible.License{}, fmt.Errorf("asking Audible for a license: %w", err)
}

// download fetches the license's file to part, resuming what a stopped or cut
// attempt left, with a new license once when the URL has expired. It returns
// the license the file came under (its key decrypts it).
func (h *Importer) download(ctx context.Context, account, asin string, lic audible.License, part, mount, rel string,
	rep *jobs.Reporter) (audible.License, error) {
	relicensed := false
	opts := audible.DownloadOptions{
		// The free-space checks, once the size is known: their refusal is the
		// job's disk_full (a free space that can't be read is no refusal).
		BeforeWrite: func(total, held int64) error {
			if total <= 0 {
				return nil
			}
			need := ceil(tmpHeadroom*float64(total)) - held
			if free, err := h.o.Free(h.o.TmpDir); err == nil && free < need {
				return fail(CodeDiskFull, "Not enough free space to back the book up: the Bridge's data folder has %s free and needs about %s for this book.",
					size(free), size(need))
			}
			need = ceil(transfer.DefaultHeadroom * float64(total))
			if free, err := h.o.Free(transfer.NearestDir(mount, rel)); err == nil && free < need {
				return fail(CodeDiskFull, "Not enough free space to back the book up: the library has %s free and needs about %s for this book.",
					size(free), size(need))
			}
			return nil
		},
		OnProgress: func(copied, total int64) {
			h.report(rep, jobs.Progress{Step: StepDownload, Code: jobs.SayDownloadingAudible, Done: int(copied), Total: int(max(total, 0))})
		},
	}
	failures := 0 // attempts in a row that failed adding nothing to the partial
	for {
		opts.ExpectedSize = lic.Size
		before := partSize(part)
		err := h.o.Accounts.Download(ctx, account, lic.URL, part, opts)
		status := audible.HTTPStatus(err)
		var f *Failure
		switch {
		case err == nil:
			return lic, nil
		case ctx.Err() != nil:
			return audible.License{}, ctx.Err()
		case errors.As(err, &f):
			return audible.License{}, f
		case fsutil.IsNoSpace(err):
			return audible.License{}, fail(CodeDiskFull, "The Bridge's data folder ran out of space during the download.")
		case errors.Is(err, audible.ErrTooLarge):
			return audible.License{}, fmt.Errorf("the book's download is larger than the Bridge's cap (%s)", size(audible.DefaultMaxDownloadBytes))
		case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusGone:
			// The presigned URL expired (a long wait, a restart): license again once.
			if relicensed {
				return audible.License{}, fail(CodeURLExpired, "Audible's download link expired again after a new license (HTTP %d): try again later.", status)
			}
			relicensed = true
			h.report(rep, jobs.Progress{Step: StepLicense, Code: jobs.SayRenewingLicense})
			if lic, err = h.license(ctx, account, asin); err != nil {
				return audible.License{}, err
			}
			h.report(rep, jobs.Progress{Step: StepDownload, Code: jobs.SayDownloadingAudible})
			failures = 0
			continue
		case status == 0 || status >= 500:
			// A dropped connection, a stall or a server error: the partial
			// resumes. Only attempts in a row that add nothing to it count, so
			// a long download over a link that drops now and then finishes.
			if partSize(part) > before {
				failures = 0
			}
			if failures++; failures < downloadAttempts {
				h.o.Log.Info("audible: a download attempt failed; resuming", "account", account, "asin", asin, "attempt", failures, "err", err)
				if werr := h.o.Wait(ctx, time.Duration(failures*failures)*2*time.Second); werr != nil {
					return audible.License{}, werr
				}
				continue
			}
		}
		if status != 0 {
			return audible.License{}, fmt.Errorf("the download from Audible answered HTTP %d", status)
		}
		return audible.License{}, fmt.Errorf("downloading from Audible: %w", err)
	}
}

// partSize is how much of the file the partial at part holds (0: none).
func partSize(part string) int64 {
	if st, err := os.Stat(part); err == nil {
		return st.Size()
	}
	return 0
}

// ranOutOfRoom reports whether a decrypt failed for want of space: ffmpeg says
// so only in its words ("No space left on device", "Disk quota exceeded").
func ranOutOfRoom(err error) bool {
	msg := strings.ToLower(err.Error())
	return fsutil.IsNoSpace(err) || strings.Contains(msg, "no space left") || strings.Contains(msg, "quota exceeded")
}

// rescanAttempts bounds the rescan's tries (the file is placed either way).
const rescanAttempts = 3

// rescan asks the server to index the placed file and returns the book's
// path, trying again a couple of times (a server busy or restarting).
func (h *Importer) rescan(ctx context.Context, srv ImportServer, libraryID int64, rel string) (string, error) {
	var err error
	for attempt := 1; attempt <= rescanAttempts; attempt++ {
		var book string
		if book, err = srv.Rescan(ctx, libraryID, rel); err == nil {
			if book == "" {
				book = path.Dir(rel)
			}
			return book, nil
		}
		if _, limited := link.RateLimited(err); limited || ctx.Err() != nil || attempt == rescanAttempts {
			break
		}
		if werr := h.o.Wait(ctx, time.Duration(attempt)*5*time.Second); werr != nil {
			return "", werr
		}
	}
	return "", err
}

// Destination is where the title goes in the library, library-relative and
// clean: the console's dest_path when set, else the account's template when
// set, else Auto (placement: series siblings' folder and shortcode, else
// Author/Title), with siblings and author folders from the library's books.
// A dest_path or template whose last segment ends in .m4b, .m4a or .mp4 names
// the file; another audio extension is refused (the backup is always an MP4);
// anything else names the folder and the file is the title's ("<Title>.m4b").
// An unusable result (a "..", an absolute path, a hidden name) is
// not_writable.
func Destination(p ImportParams, rec source.Record, books []link.LibraryBook) (string, error) {
	item, err := destination(p, rec, importjob.NewPlanner(books))
	return item.RelPath, err
}

// destination is Destination over the library's books in a planner (a poll
// plans many titles over one), with the plan's Kind and Siblings, which an
// error keeps (RelPath is then ""). A dest_path is no plan: only its RelPath
// is set.
func destination(p ImportParams, rec source.Record, planner *importjob.Planner) (importjob.PlannedItem, error) {
	var item importjob.PlannedItem
	rel := strings.TrimSpace(p.DestPath)
	if rel == "" {
		mode, tmpl := placement.ModeAuto, strings.TrimSpace(p.Template)
		if tmpl != "" {
			mode = placement.ModeTemplate
		}
		planned, err := planner.Plan(rec, mode, tmpl)
		if err != nil {
			return planned, fail(CodeNotWritable, "The account's path template can't place the title: %v", err)
		}
		// A title without an author leaves Auto an empty first folder
		// ("/Title/Title.m4b"): it goes, as a template's empty folders do.
		item, rel = planned, strings.TrimLeft(planned.RelPath, "/")
		if tmpl == "" {
			item.RelPath, err = cleanDest(rel) // Auto names the file
			return item, err
		}
	}
	item.RelPath = ""
	switch ext := strings.ToLower(path.Ext(rel)); {
	case mp4Names[ext]:
	case otherAudio[ext]:
		return item, fail(CodeNotWritable, "The destination %q names a %s file, but the backup is an M4B: end it in .m4b, or name a folder.", rel, ext)
	default:
		rel = path.Join(rel, rec.FileName)
	}
	var err error
	item.RelPath, err = cleanDest(rel)
	return item, err
}

// cleanDest is rel through transfer.CleanRel, refused as not_writable.
func cleanDest(rel string) (string, error) {
	clean, err := transfer.CleanRel(rel)
	if err != nil {
		return "", fail(CodeNotWritable, "The destination %q isn't a valid path in the library.", rel)
	}
	return clean, nil
}

// mp4Names are the file names a destination may give the backup (ffmpeg's
// MP4, -f ipod); otherAudio are the audio files it can't be.
var (
	mp4Names   = map[string]bool{".m4b": true, ".m4a": true, ".mp4": true}
	otherAudio = map[string]bool{".mp3": true, ".aac": true, ".flac": true, ".ogg": true, ".opus": true, ".wav": true,
		".wma": true, ".mka": true, ".m4p": true, ".aax": true, ".aaxc": true}
)

func ceil(v float64) int64 {
	if v >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Ceil(v))
}

// size is a byte count for a sentence (MB/GB, one decimal).
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}

// PlacementFolder locates a library directory named by an import's private record.
type PlacementFolder struct {
	Server    string
	LibraryID int64
	Dir       string
}

func recordedFolder(file, server string) (PlacementFolder, bool) {
	var m stagedBook
	b, err := os.ReadFile(file) //nolint:gosec // regular metadata file in the private import folder
	if err != nil || json.Unmarshal(b, &m) != nil || m.LibraryID <= 0 {
		return PlacementFolder{}, false
	}
	rel, err := transfer.CleanRel(m.Path)
	if err != nil {
		return PlacementFolder{}, false
	}
	if m.Server == "" && strings.HasSuffix(file, placedSuffix) {
		m.Server = server
	}
	return PlacementFolder{Server: m.Server, LibraryID: m.LibraryID, Dir: path.Dir(rel)}, true
}

// SweepTmp is the start-up sweep of the import's temp folder (decision 6): a
// partial download (*.aaxc.part and its .state) younger than PartialMaxAge
// stays for its job to resume. A complete decrypt or placement marker as young
// stays too. An interrupted decrypt has a different suffix and is discarded.
// Before expiring records, collect the placement folders for the library sweep.
// Placement markers carry only a library ID, scoped here to the paired server.
func SweepTmp(dir string, now time.Time, server string) ([]PlacementFolder, error) {
	var folders []PlacementFolder
	keep := map[string]bool{}
	inspect := func(file string, e fs.DirEntry) {
		name := e.Name()
		marker, part, ready := strings.HasSuffix(name, placedSuffix), strings.HasSuffix(name, ".aaxc.part"), strings.HasSuffix(name, readySuffix)
		if marker || strings.HasSuffix(name, stagedMeta(readySuffix)) {
			if f, ok := recordedFolder(file, server); ok {
				folders = append(folders, f)
			}
		}
		if !marker && !part && !ready {
			return
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) < PartialMaxAge {
			keep[e.Name()] = true
			if ready {
				keep[stagedMeta(name)] = true
			}
			if part {
				keep[filepath.Base(audible.StatePath(e.Name()))] = true
			}
		}
	}
	err := transfer.PrivateDir(dir).Sweep(inspect, func(file string, e fs.DirEntry) error {
		if keep[e.Name()] {
			return nil
		}
		return os.RemoveAll(file)
	})
	return folders, err
}
