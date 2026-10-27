package toolfetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/kodestar/audiosilo-bridge/internal/execfile"
	"github.com/kodestar/audiosilo-bridge/internal/execfile/forktest"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeTool writes an executable stub named tool into dir and returns its path.
func fakeTool(t *testing.T, dir, tool string) string {
	t.Helper()
	p := filepath.Join(dir, binName(tool))
	if err := execfile.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSpecFor(t *testing.T) {
	for _, p := range []struct{ os, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
	} {
		if _, ok := specFor(p.os, p.arch); !ok {
			t.Errorf("specFor(%s/%s) ok=false, want a source", p.os, p.arch)
		}
	}
	if _, ok := specFor("plan9", "mips"); ok {
		t.Error("specFor(unsupported) ok=true, want false")
	}
}

func TestCached(t *testing.T) {
	dir := t.TempDir()
	if fm, fp := cached(dir); fm != "" || fp != "" {
		t.Fatalf("empty dir: cached=%q,%q want empty", fm, fp)
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		fakeTool(t, dir, tool)
	}
	fm, fp := cached(dir)
	if fm == "" || fp == "" {
		t.Fatalf("cached after writing tools = %q,%q want both set", fm, fp)
	}
}

// ensure must short-circuit (no download) when both tools are already cached AND
// pass their self-check. The stubs here `exit 0` on `-version`, so they verify.
func TestEnsureUsesCache(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		fakeTool(t, dir, tool)
	}
	fm, fp := ensure(context.Background(), dir, discard())
	if fm != filepath.Join(dir, binName("ffmpeg")) || fp != filepath.Join(dir, binName("ffprobe")) {
		t.Fatalf("ensure with cache = %q,%q want the cached paths", fm, fp)
	}
}

// resolveLocal must honor an explicit path, and reject an explicit-but-missing one
// WITHOUT falling back to $PATH.
func TestResolveLocalExplicit(t *testing.T) {
	dir := t.TempDir()
	ff := fakeTool(t, dir, "ffmpeg")
	if got := resolveLocal("ffmpeg", ff); got != ff {
		t.Errorf("explicit path: got %q want %q", got, ff)
	}
	if got := resolveLocal("ffmpeg", filepath.Join(dir, "nope")); got != "" {
		t.Errorf("missing explicit path: got %q want empty", got)
	}
}

// resolveLocal falls back to $PATH by the tool's own name when no explicit path is
// given (the daemon-adjacent copy is absent in a test binary's dir).
func TestResolveLocalPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH stub uses a shell script")
	}
	dir := t.TempDir()
	want := fakeTool(t, dir, "ffprobe")
	t.Setenv("PATH", dir)
	if got := resolveLocal("ffprobe", ""); got != want {
		t.Errorf("PATH resolution: got %q want %q", got, want)
	}
}

// Resolve with auto-download off and no local tools yields empty paths (a graceful
// degrade), and must not attempt a network fetch.
func TestResolveNoAutoDownload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir) // empty dir: nothing resolvable
	tools := Resolve(context.Background(), ResolveConfig{AutoDownload: false}, dir, discard())
	if tools.FFmpeg != "" || tools.FFprobe != "" {
		t.Errorf("Resolve(auto_download=false) = %+v, want empty", tools)
	}
}

// Resolve prefers explicit configured paths over everything else.
func TestResolveExplicit(t *testing.T) {
	dir := t.TempDir()
	ff := fakeTool(t, dir, "ffmpeg")
	fp := fakeTool(t, dir, "ffprobe")
	tools := Resolve(context.Background(), ResolveConfig{FFmpegPath: ff, FFprobePath: fp}, t.TempDir(), discard())
	if tools.FFmpeg != ff || tools.FFprobe != fp {
		t.Errorf("Resolve explicit = %+v, want %q,%q", tools, ff, fp)
	}
}

// extractZip must pull only ffmpeg/ffprobe (by basename, from any subdir) and mark
// them executable, ignoring everything else - and never escape destDir.
func TestExtractZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		"ffmpeg-build/bin/" + binName("ffmpeg"):  "FFMPEG",
		"ffmpeg-build/bin/" + binName("ffprobe"): "FFPROBE",
		"ffmpeg-build/README.txt":                "ignore me",
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractZip(&buf, dir); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	for tool, want := range map[string]string{"ffmpeg": "FFMPEG", "ffprobe": "FFPROBE"} {
		p := filepath.Join(dir, binName(tool))
		got, err := os.ReadFile(p) //nolint:gosec // test-controlled path
		if err != nil {
			t.Fatalf("expected %s extracted: %v", tool, err)
		}
		if string(got) != want {
			t.Errorf("%s body = %q, want %q", tool, got, want)
		}
		if info, _ := os.Stat(p); info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable (mode %v)", tool, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err == nil {
		t.Error("extractZip wrote a non-tool file")
	}
}

// A zip-slip entry name (../.. escaping the archive root) must NOT write outside
// destDir - extraction is by basename, so the escape is neutralized.
func TestExtractZipSlipNeutralized(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// A malicious name that would escape if joined verbatim; its basename is a real
	// tool name, so it lands safely inside destDir and nowhere else.
	entries := map[string]string{
		"../../../../evil-" + binName("ffmpeg"):        "SLIP",    // non-tool basename: ignored
		"../../../../../../tmp/x/" + binName("ffmpeg"): "FFMPEG",  // tool basename: destDir only
		"deep/../../" + binName("ffprobe"):             "FFPROBE", // traversal collapses to basename
		"note.txt":                                     "ignore",
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := extractZip(&buf, dir); err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	// The tool binaries exist only inside destDir; nothing escaped to a parent.
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := os.Stat(filepath.Join(dir, binName(tool))); err != nil {
			t.Errorf("%s not extracted into destDir: %v", tool, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil-"+binName("ffmpeg"))); err == nil {
		t.Error("a zip-slip entry escaped destDir")
	}
}

// An over-cap zip archive is rejected before any extraction (shrunk cap so the
// test needs no 300 MiB fixture).
func TestExtractZipOversizeRejected(t *testing.T) {
	restore := maxToolBytes
	maxToolBytes = 16
	defer func() { maxToolBytes = restore }()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(binName("ffmpeg"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "a real zip is larger than sixteen bytes"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractZip(&buf, t.TempDir()); err == nil {
		t.Error("extractZip accepted an archive over the cap")
	}
}

// buildTarXz builds an in-memory .tar.xz from name->body entries (regular files,
// 0755) using the xz writer, so tests need no external tooling.
func buildTarXz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(xw)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// extractTarXz pulls only the ffmpeg/ffprobe binaries (by basename, from any
// subdir) and marks them executable, ignoring everything else.
func TestExtractTarXz(t *testing.T) {
	data := buildTarXz(t, map[string]string{
		"ffmpeg-build/bin/" + binName("ffmpeg"):  "FFMPEG",
		"ffmpeg-build/bin/" + binName("ffprobe"): "FFPROBE",
		"ffmpeg-build/README.txt":                "ignore me",
	})
	dir := t.TempDir()
	if err := extractTarXz(bytes.NewReader(data), dir); err != nil {
		t.Fatalf("extractTarXz: %v", err)
	}
	for tool, want := range map[string]string{"ffmpeg": "FFMPEG", "ffprobe": "FFPROBE"} {
		p := filepath.Join(dir, binName(tool))
		got, err := os.ReadFile(p) //nolint:gosec // test-controlled path
		if err != nil {
			t.Fatalf("expected %s extracted: %v", tool, err)
		}
		if string(got) != want {
			t.Errorf("%s body = %q, want %q", tool, got, want)
		}
		if info, _ := os.Stat(p); info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable (mode %v)", tool, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err == nil {
		t.Error("extractTarXz wrote a non-tool file")
	}
}

// A tar entry that traverses out of the archive root is rejected outright.
func TestExtractTarXzTraversalRejected(t *testing.T) {
	data := buildTarXz(t, map[string]string{
		"../../../../etc/evil": "PWNED",
	})
	dir := t.TempDir()
	if err := extractTarXz(bytes.NewReader(data), dir); err == nil {
		t.Error("extractTarXz accepted a traversing entry")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "etc", "evil")); err == nil {
		t.Error("a traversing tar entry escaped destDir")
	}
}

// An over-cap compressed tar.xz download is rejected by fetchArchive before it is
// decoded (shrunk cap; the served body need not be valid xz - the cap trips
// first).
func TestFetchArchiveTarXzOversizeRejected(t *testing.T) {
	restore := maxToolBytes
	maxToolBytes = 8
	defer func() { maxToolBytes = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this body is comfortably larger than eight bytes"))
	}))
	defer srv.Close()

	if err := fetchArchive(context.Background(), srv.URL, "tar.xz", t.TempDir()); err == nil {
		t.Error("fetchArchive accepted an over-cap tar.xz download")
	}
}

// TestEnsureDownloadsAndVerifies exercises ensure's full download branch end to
// end: platformSpec points at an httptest-served tar.xz holding executable stub
// tools; ensure downloads, extracts, and adopts them only after the `-version`
// self-check passes. Unix-only (the stub is a shell script).
func TestEnsureDownloadsAndVerifies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub tool is a shell script")
	}
	stub := "#!/bin/sh\nexit 0\n"
	data := buildTarXz(t, map[string]string{
		"ffmpeg-build/bin/" + binName("ffmpeg"):  stub,
		"ffmpeg-build/bin/" + binName("ffprobe"): stub,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	restore := platformSpec
	platformSpec = func(string, string) (spec, bool) {
		return spec{combinedURL: srv.URL, combinedKind: "tar.xz"}, true
	}
	defer func() { platformSpec = restore }()

	dir := t.TempDir()
	fm, fp := ensure(context.Background(), dir, discard())
	if fm != filepath.Join(dir, binName("ffmpeg")) || fp != filepath.Join(dir, binName("ffprobe")) {
		t.Fatalf("ensure downloaded = %q,%q, want the two extracted tool paths", fm, fp)
	}
	for _, p := range []string{fm, fp} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm()&0o100 == 0 {
			t.Errorf("downloaded tool %q not executable: %v", p, err)
		}
	}
}

// A download that fails still adopts only tools that pass their self-check: a
// cached one that doesn't run comes back "" (and is removed), one that does is
// kept. (A failed download can leave a tool copyExec just wrote; its first run
// must be the self-check, which retries a run refused as busy.)
func TestEnsureFailedDownloadVerifiesWhatIsCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub tool is a shell script")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	restore := platformSpec
	platformSpec = func(string, string) (spec, bool) {
		return spec{combinedURL: srv.URL, combinedKind: "tar.xz"}, true
	}
	defer func() { platformSpec = restore }()

	dir := t.TempDir()
	broken := filepath.Join(dir, binName("ffmpeg"))
	if err := execfile.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fm, fp := ensure(context.Background(), dir, discard())
	if fm != "" || fp != "" {
		t.Fatalf("a failed download with a broken cached ffmpeg = %q,%q, want none", fm, fp)
	}
	if _, err := os.Stat(broken); !os.IsNotExist(err) {
		t.Errorf("the broken ffmpeg was kept: %v", err)
	}

	good := fakeTool(t, dir, "ffmpeg")
	if fm, _ := ensure(context.Background(), dir, discard()); fm != good {
		t.Fatalf("a failed download with a working cached ffmpeg = %q, want %q", fm, good)
	}
}

// A self-check cut short by shutdown keeps the tool: a cancelled context says
// nothing about the file (ensure checks what a cancelled download left).
func TestVerifiedKeepsToolWhenCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub tool is a shell script")
	}
	good := fakeTool(t, t.TempDir(), "ffmpeg")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := verified(ctx, good, discard()); got != "" {
		t.Fatalf("verified with a cancelled context = %q, want \"\"", got)
	}
	if _, err := os.Stat(good); err != nil {
		t.Errorf("the tool was removed: %v", err)
	}
}

// bridge#12/#16: a tool binary copyExec just installed (a plain copy, no fork
// lock) passes its self-check, its first run, while other goroutines fork (each
// one's exec is the others' fork): a run refused as busy is tried again, never
// failed. No temp file is left behind. Each source is written the unlocked way
// (writeMode), as the extraction does; the run is of the copy.
func TestCopyExecSelfCheckUnderParallelForks(t *testing.T) {
	dir := forktest.Run(t, func(p string) (string, error) {
		src := p + ".src"
		if _, err := writeMode(src, strings.NewReader(forktest.Script), 0o644); err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(src) }()
		return p, copyExec(src, p)
	}, func(p string) error {
		return runSelfCheck(context.Background(), p, nil, 10*time.Second, discard())
	})
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("%d files left behind, first %q", len(left), left[0].Name())
	}
}

// writeMode installs exactly the one file with its mode and bytes, executable or
// not, and on a failed read installs nothing and leaves no temp file.
func TestWriteModeInstallsOnlyTheFile(t *testing.T) {
	for _, perm := range []os.FileMode{0o755, 0o644} {
		dir := t.TempDir()
		p := filepath.Join(dir, "f")
		n, err := writeMode(p, strings.NewReader("BODY"), perm)
		if err != nil || n != 4 {
			t.Fatalf("writeMode(%v) = %d, %v", perm, n, err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != perm {
			t.Errorf("mode = %v, want %v", info.Mode().Perm(), perm)
		}
		if b, _ := os.ReadFile(p); string(b) != "BODY" { //nolint:gosec // the test's own file
			t.Errorf("content = %q", b)
		}
		if left, _ := os.ReadDir(dir); len(left) != 1 {
			t.Errorf("perm %v: %d files in the dir, want 1", perm, len(left))
		}

		failDir := t.TempDir()
		if _, err := writeMode(filepath.Join(failDir, "f"), io.MultiReader(strings.NewReader("x"), iotest.ErrReader(io.ErrUnexpectedEOF)), perm); err == nil {
			t.Fatalf("perm %v: writeMode succeeded with a failing reader", perm)
		}
		if left, _ := os.ReadDir(failDir); len(left) != 0 {
			t.Errorf("perm %v: a failed write left %d files", perm, len(left))
		}
	}
}

// bridge#12: a tool still open for writing when its self-check runs (a child
// forked during its extraction holds the descriptor until it execs; here the
// test holds it) is refused with ETXTBSY on Linux. Allowed: released while the
// self-check retries, it passes. Denied: held throughout, the self-check fails.
func TestSelfCheckRetriesABusyBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	restore := busyRetries
	busyRetries = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond}
	defer func() { busyRetries = restore }()
	p := fakeTool(t, t.TempDir(), "ffmpeg")
	w, err := os.OpenFile(p, os.O_WRONLY, 0) //nolint:gosec // the test's own tool
	if err != nil {
		t.Fatal(err)
	}
	release := time.AfterFunc(50*time.Millisecond, func() { _ = w.Close() })
	defer release.Stop()
	if err := runSelfCheck(context.Background(), p, nil, 10*time.Second, discard()); err != nil {
		t.Fatalf("self-check of a tool released while it retries: %v", err)
	}

	if runtime.GOOS != "linux" {
		return // only Linux refuses to run a file that is open for writing
	}
	held, err := os.OpenFile(p, os.O_WRONLY, 0) //nolint:gosec // the test's own tool
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := runSelfCheck(context.Background(), p, nil, 10*time.Second, discard()); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("self-check of a tool held open throughout = %v, want ETXTBSY", err)
	}
}

// bridge#12: a whisper-cli still busy after the self-check's retries is not a
// "won't run here" failure. Denied: errWhisperSelfCheck (it would adopt the CPU
// build as a sticky fallback). Allowed: the ETXTBSY error, so the install fails
// and is tried again.
func TestWhisperSelfCheckBusyIsNotAFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux refuses to run a file that is open for writing")
	}
	restore := busyRetries
	busyRetries = []time.Duration{10 * time.Millisecond}
	defer func() { busyRetries = restore }()
	p := fakeTool(t, t.TempDir(), "whisper-cli")
	held, err := os.OpenFile(p, os.O_WRONLY, 0) //nolint:gosec // the test's own tool
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	err = whisperSelfCheck(context.Background(), p, discard())
	if !errors.Is(err, syscall.ETXTBSY) || errors.Is(err, errWhisperSelfCheck) {
		t.Fatalf("self-check of a busy whisper-cli = %v, want ETXTBSY and not errWhisperSelfCheck", err)
	}
}
