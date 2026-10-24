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
		public, scheme, host, want string
	}{
		{"http://192.168.1.20:8091/", "http", "10.0.0.1:8091", "http://192.168.1.20:8091"},
		{"https://bridge.lan/sub/", "http", "x", "https://bridge.lan/sub"},
		{"", "http", "10.0.0.1:8091", "http://10.0.0.1:8091"},
		{"", "http", "[fd00::1]:8091", "http://[fd00::1]:8091"},
		{"", "https", "nas.local", "https://nas.local"},
		// A public address that can't go into a script falls back to the request's.
		{"ftp://nas", "http", "nas:8091", "http://nas:8091"},
		{"http://user:pw@nas:8091", "http", "nas:8091", "http://nas:8091"},
		{"http://nas:8091/?a=1", "http", "nas:8091", "http://nas:8091"},
		{"http://nas:8091/a'b", "http", "nas:8091", "http://nas:8091"},
		{"http://na's:8091", "http", "nas:8091", "http://nas:8091"},
	} {
		got, err := Address(tc.public, tc.scheme, tc.host)
		if err != nil || got != tc.want {
			t.Errorf("Address(%q, %q, %q) = %q, %v, want %q", tc.public, tc.scheme, tc.host, got, err, tc.want)
		}
	}
	for _, host := range []string{"", "a b", "evil';rm -rf ~;'", "nas:80:80", "$(id)", "nas\n"} {
		if got, err := Address("", "http", host); !errors.Is(err, ErrAddress) {
			t.Errorf("Address(host %q) = %q, %v", host, got, err)
		}
	}
	if _, err := Address("", "file", "nas"); !errors.Is(err, ErrAddress) {
		t.Errorf("a file scheme = %v", err)
	}
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
	run := func(t *testing.T, args ...string) (string, string, string, error) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "bin")
		ran, urls := filepath.Join(tmp, t.Name()[len("TestScriptRuns/"):]+".ran"), filepath.Join(tmp, t.Name()[len("TestScriptRuns/"):]+".url")
		cmd := exec.Command("sh", append([]string{"-s", "--", "--dir", dir}, args...)...)
		cmd.Stdin = strings.NewReader(text)
		cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "SERVED="+served, "RAN="+ran, "URLS="+urls, "HOME="+tmp)
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
		out, ran, url, err := run(t, "--token", "asw_abc")
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
		out, ran, _, err := run(t, "--token=asw_def", "--run")
		if err != nil || ran != "worker --bridge http://192.168.1.20:8091 --token asw_def" {
			t.Fatalf("ran %q, %v\n%s", ran, err, out)
		}
	})
	t.Run("notoken", func(t *testing.T) {
		out, ran, url, err := run(t, "--run")
		if err == nil || !strings.Contains(out, "--token is required") || ran != "" || url != "" {
			t.Fatalf("no token: %v, ran %q, url %q\n%s", err, ran, url, out)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		out, _, url, err := run(t, "--token", "asw_x", "--service")
		if err == nil || !strings.Contains(out, "unknown option: --service") || url != "" {
			t.Fatalf("unknown flag: %v\n%s", err, out)
		}
	})
}
