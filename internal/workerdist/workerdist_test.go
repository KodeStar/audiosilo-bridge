package workerdist

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "audiosilo-bridge_linux_amd64.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A folder where a build would be isn't one.
	if err := os.Mkdir(filepath.Join(dir, "audiosilo-bridge_linux_arm64.gz"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := Dist{Dir: dir}
	f, info, err := d.Open("linux", "amd64")
	if err != nil || info.Name() != "audiosilo-bridge_linux_amd64.gz" {
		t.Fatalf("open = %v, %v", info, err)
	}
	_ = f.Close()
	for _, tc := range []struct {
		os, arch string
		want     error
	}{
		{"linux", "arm64", ErrMissing},
		{"darwin", "arm64", ErrMissing},
		{"windows", "amd64", ErrUnknownPlatform},
		{"linux", "386", ErrUnknownPlatform},
		{"..", "..", ErrUnknownPlatform},
		{"linux/../..", "amd64", ErrUnknownPlatform},
	} {
		if _, _, err := d.Open(tc.os, tc.arch); !errors.Is(err, tc.want) {
			t.Errorf("open %s/%s = %v, want %v", tc.os, tc.arch, err, tc.want)
		}
	}
	if !d.Available() {
		t.Error("a folder with a build isn't available")
	}
	for _, empty := range []Dist{{}, {Dir: filepath.Join(dir, "none")}, {Dir: t.TempDir()}} {
		if empty.Available() {
			t.Errorf("%+v is available", empty)
		}
		if _, _, err := empty.Open("linux", "amd64"); !errors.Is(err, ErrMissing) {
			t.Errorf("%+v open = %v", empty, err)
		}
	}
}

func TestAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		scheme, host, want string
	}{
		{"http", "10.0.0.1:8091", "http://10.0.0.1:8091"},
		{"http", "[fd00::1]:8091", "http://[fd00::1]:8091"},
		{"https", "nas.local", "https://nas.local"},
		{"http", "my_nas:8091", "http://my_nas:8091"},
	} {
		got, err := Address(tc.scheme, tc.host)
		if err != nil || got != tc.want {
			t.Errorf("Address(%q, %q) = %q, %v, want %q", tc.scheme, tc.host, got, err, tc.want)
		}
	}
	for _, host := range []string{"", "a b", "evil';rm -rf ~;'", "nas:80:80", "$(id)", "nas\n", "user@nas", "nas/x"} {
		if got, err := Address("http", host); !errors.Is(err, ErrAddress) {
			t.Errorf("Address(host %q) = %q, %v", host, got, err)
		}
	}
	if _, err := Address("file", "nas"); !errors.Is(err, ErrAddress) {
		t.Errorf("a file scheme = %v", err)
	}
}

// An error script says why on stderr and exits 1, whatever the message holds
// (quotes, a command substitution, a newline): it is printed, never run.
func TestErrorScript(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, tc := range []struct{ msg, want string }{
		{ErrMissing.Error(), ErrMissing.Error()},
		{"it's $(touch pwned) `id` \"x\"\nline two\r", "it's $(touch pwned) `id` \"x\"\nline two "},
		// printf's own syntax and a quote-escape lookalike stay literal.
		{`-n 100% \\ \n %s '\'' ; touch pwned`, `-n 100% \\ \n %s '\'' ; touch pwned`},
		// Control characters (an escape sequence, a NUL, a tab, a DEL) become spaces.
		{"esc \x1b[2J nul \x00 tab \t del \x7f end", "esc  [2J nul   tab   del   end"},
	} {
		text := ErrorScript(tc.msg)
		if !strings.HasPrefix(text, "#!/bin/sh\n") {
			t.Fatalf("script:\n%s", text)
		}
		dir := t.TempDir()
		stderr, code := runScript(t, text, dir)
		want := "audiosilo-bridge worker: " + tc.want + "\n"
		if code != 1 || stderr != want {
			t.Fatalf("exit %d, stderr %q, want %q", code, stderr, want)
		}
		if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
			t.Fatal("the message ran a command")
		}
	}
}

// runScript runs a script with sh in dir: its stderr and exit code.
func runScript(t *testing.T, text, dir string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return stderr.String(), cmd.ProcessState.ExitCode()
}

// The served script carries the address once, single-quoted, parses as POSIX
// sh, and holds no placeholder left over.
func TestScript(t *testing.T) {
	t.Parallel()
	text, err := Script("http://192.168.1.20:8091")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "\nBRIDGE='http://192.168.1.20:8091'\n") || strings.Contains(text, "@@") {
		t.Fatalf("script:\n%s", text)
	}
	for _, flag := range []string{"--token)", "--run)", "--dir)"} {
		if !strings.Contains(text, flag) {
			t.Errorf("the script lacks %s", flag)
		}
	}
	if strings.Contains(text, "\u2014") {
		t.Error("an em dash in the script")
	}
	if !strings.Contains(text, "\nRELEASES='"+ReleasesURL+"'\n") {
		t.Error("the script's releases address isn't ReleasesURL")
	}
	if _, err := Script("http://a'b"); !errors.Is(err, ErrAddress) {
		t.Errorf("a quote = %v", err)
	}
	if _, err := Script(""); !errors.Is(err, ErrAddress) {
		t.Errorf("empty = %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// The script end to end, on this computer, with a fake curl serving a fake
// program: it downloads from the Bridge's address, installs the program in
// --dir, and runs `worker install` (or `worker`, with --run) with the token.
func TestScriptRuns(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the script runs on macOS and Linux")
	}
	for _, tool := range []string{"sh", "gzip", "uname", "mv", "chmod"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	text, err := Script("http://192.168.1.20:8091")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	fakeBin := filepath.Join(tmp, "fakebin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The program the "Bridge" serves: says its version, and records how it ran.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo v9.9.9; exit 0; fi\necho \"$*\" > \"$RAN\"\n"))
	_ = zw.Close()
	served := filepath.Join(tmp, "served.gz")
	if err := os.WriteFile(served, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// curl -fsSL -o <out> <url>: copies the served file, records the URL.
	curl := "#!/bin/sh\nout=''\nwhile [ $# -gt 0 ]; do case \"$1\" in -o) out=$2; shift 2;; -*) shift;; *) url=$1; shift;; esac; done\n" +
		"echo \"$url\" > \"$URLS\"\ncp \"$SERVED\" \"$out\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(curl), 0o755); err != nil { //nolint:gosec // a test's fake tool
		t.Fatal(err)
	}
	// run runs script with args; saved is a token the worker's data dir already
	// holds ("" for none: a first install).
	run := func(t *testing.T, script, saved string, args ...string) (string, string, string, error) {
		t.Helper()
		sub := t.TempDir()
		dir, ran, urls := filepath.Join(sub, "bin"), filepath.Join(sub, "ran"), filepath.Join(sub, "url")
		data := filepath.Join(sub, "data")
		if saved != "" {
			if err := os.MkdirAll(data, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "worker.token"), []byte(saved+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("sh", append([]string{"-s", "--", "--dir", dir}, args...)...)
		cmd.Stdin = strings.NewReader(script)
		cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "SERVED="+served, "RAN="+ran, "URLS="+urls, "HOME="+tmp,
			"AUDIOSILO_WORKER_DATA="+data, "AUDIOSILO_WORKER_TOKEN=")
		out, err := cmd.CombinedOutput()
		r, _ := os.ReadFile(ran)
		u, _ := os.ReadFile(urls)
		if err == nil {
			if fi, serr := os.Stat(filepath.Join(dir, "audiosilo-bridge")); serr != nil || fi.Mode().Perm() != 0o755 {
				t.Fatalf("the program = %v, %v", fi, serr)
			}
		}
		return string(out), strings.TrimSpace(string(r)), strings.TrimSpace(string(u)), err
	}
	t.Run("install", func(t *testing.T) {
		out, ran, url, err := run(t, text, "", "--token", "asw_abc")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if ran != "worker install --bridge http://192.168.1.20:8091 --token asw_abc" {
			t.Fatalf("ran %q\n%s", ran, out)
		}
		if !strings.HasPrefix(url, "http://192.168.1.20:8091/worker/download/"+runtime.GOOS+"/") {
			t.Fatalf("downloaded %q", url)
		}
		if !strings.Contains(out, "Installed audiosilo-bridge v9.9.9 at ") {
			t.Fatalf("output:\n%s", out)
		}
	})
	t.Run("run", func(t *testing.T) {
		out, ran, _, err := run(t, text, "", "--token=asw_def", "--run")
		if err != nil || ran != "worker --bridge http://192.168.1.20:8091 --token asw_def" {
			t.Fatalf("ran %q, %v\n%s", ran, err, out)
		}
	})
	// Running it again to update: the token the first install saved does.
	t.Run("update", func(t *testing.T) {
		out, ran, url, err := run(t, text, "asw_saved")
		if err != nil || ran != "worker install --bridge http://192.168.1.20:8091" || url == "" {
			t.Fatalf("update: %v, ran %q, url %q\n%s", err, ran, url, out)
		}
	})
	// A download cut short runs nothing (here, past the line that installs the
	// program): the script is one function, called on its last line.
	t.Run("truncated", func(t *testing.T) {
		const line = "mv -f \"$tmp\" \"$bin\"\n"
		cut := text[:strings.Index(text, line)+len(line)]
		out, ran, url, err := run(t, cut, "", "--token", "asw_abc")
		if err == nil || ran != "" || url != "" {
			t.Fatalf("truncated: %v, ran %q, url %q\n%s", err, ran, url, out)
		}
	})
	t.Run("notoken", func(t *testing.T) {
		out, ran, url, err := run(t, text, "", "--run")
		if err == nil || !strings.Contains(out, "--token is required") || ran != "" || url != "" {
			t.Fatalf("no token: %v, ran %q, url %q\n%s", err, ran, url, out)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		out, _, url, err := run(t, text, "", "--token", "asw_x", "--service")
		if err == nil || !strings.Contains(out, "unknown option: --service") || url != "" {
			t.Fatalf("unknown flag: %v\n%s", err, out)
		}
	})
}

// The Dockerfile and release.yml build the same fixed set the routes serve
// (their shell loops can't import Platforms).
func TestPlatformsMatchBuilds(t *testing.T) {
	t.Parallel()
	list := make([]string, 0, len(Platforms))
	for _, p := range Platforms {
		list = append(list, p.OS+"/"+p.Arch)
	}
	loop := "for p in " + strings.Join(list, " ") + "; do"
	for _, f := range []string{"../../Dockerfile", "../../.github/workflows/release.yml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), loop) {
			t.Errorf("%s doesn't build %q", f, loop)
		}
	}
}
