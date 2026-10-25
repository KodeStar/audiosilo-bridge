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

	"github.com/kodestar/audiosilo-bridge/internal/worker"
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
		got, err := Address(tc.scheme, tc.host, "")
		if err != nil || got != tc.want {
			t.Errorf("Address(%q, %q) = %q, %v, want %q", tc.scheme, tc.host, got, err, tc.want)
		}
	}
	// A hostile Host is refused before any public address is looked at, even
	// one it would otherwise "match".
	for _, host := range []string{"", "a b", "evil';rm -rf ~;'", "nas:80:80", "$(id)", "nas\n", "user@nas", "nas/x"} {
		for _, pub := range []string{"", "http://nas", "http://" + host} {
			if got, err := Address("http", host, pub); !errors.Is(err, ErrAddress) {
				t.Errorf("Address(host %q, public %q) = %q, %v", host, pub, got, err)
			}
		}
	}
	if _, err := Address("file", "nas", "http://nas"); !errors.Is(err, ErrAddress) {
		t.Errorf("a file scheme = %v", err)
	}
}

// The public address goes in, as set, when the request reached its host and
// port; otherwise the request's own address does.
func TestAddressPublicURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		scheme, host, pub, want string
	}{
		// Matches: the same host and port (a default port filled in on either
		// side), any case; the path prefix and scheme kept, trailing slashes not.
		{"http", "192.168.1.20:8091", "http://192.168.1.20:8091/", "http://192.168.1.20:8091"},
		{"http", "nas.lan:8091", " http://NAS.lan:8091 ", "http://NAS.lan:8091"},
		{"http", "proxy.lan", "http://proxy.lan/bridge//", "http://proxy.lan/bridge"},
		{"http", "proxy.lan:80", "http://proxy.lan/bridge", "http://proxy.lan/bridge"},
		{"http", "proxy.lan", "http://proxy.lan:80/a/b-c_d.e~f%20", "http://proxy.lan:80/a/b-c_d.e~f%20"},
		{"http", "[fd00::1]:8091", "http://[FD00::1]:8091/x", "http://[FD00::1]:8091/x"},
		// Behind a TLS proxy: the request is plain http with the client's Host.
		{"http", "bridge.example.com", "https://bridge.example.com/bridge", "https://bridge.example.com/bridge"},
		{"http", "bridge.example.com:443", "https://bridge.example.com/bridge/", "https://bridge.example.com/bridge"},
		{"https", "bridge.example.com", "https://bridge.example.com", "https://bridge.example.com"},
		{"http", "proxy.lan:8443", "https://proxy.lan:8443/bridge", "https://proxy.lan:8443/bridge"},
		// Mismatches fall back to the request's address.
		{"http", "10.0.0.7:18091", "http://192.168.1.20:8091", "http://10.0.0.7:18091"},
		{"http", "192.168.1.20:8092", "http://192.168.1.20:8091", "http://192.168.1.20:8092"},
		{"http", "192.168.1.20", "http://192.168.1.20:8091", "http://192.168.1.20"},
		{"http", "proxy.lan:80", "https://proxy.lan/bridge", "http://proxy.lan:80"},
		{"http", "proxy.lan:8091", "https://proxy.lan/bridge", "http://proxy.lan:8091"},
		{"http", "nas.lan", "http://nas.lan.evil.com", "http://nas.lan"},
		{"http", "nas.lan", "http://evil.com/nas.lan", "http://nas.lan"},
		// A public address a script can't carry as set is never used.
		{"http", "nas.lan", "ftp://nas.lan", "http://nas.lan"},
		{"http", "nas.lan", "nas.lan", "http://nas.lan"},
		{"http", "nas.lan", "//nas.lan", "http://nas.lan"},
		{"http", "nas.lan", "http://user:pw@nas.lan", "http://nas.lan"},
		{"http", "nas.lan", "http://nas.lan/?x=1", "http://nas.lan"},
		{"http", "nas.lan", "http://nas.lan/#x", "http://nas.lan"},
		{"http", "nas.lan", "http://nas.lan/a'b", "http://nas.lan"},
		{"http", "nas.lan", "http://nas.lan/a b", "http://nas.lan"},
		{"http", "nas.lan", "http://nas.lan/$(id)", "http://nas.lan"},
		{"http", "nas.lan", "http://nas.lan/a\nb", "http://nas.lan"},
		{"http", "nas.lan", "http://nas.lan/%zz", "http://nas.lan"},
		{"http", "nas.lan", "http:nas.lan", "http://nas.lan"},
	} {
		got, err := Address(tc.scheme, tc.host, tc.pub)
		if err != nil || got != tc.want {
			t.Errorf("Address(%q, %q, public %q) = %q, %v, want %q", tc.scheme, tc.host, tc.pub, got, err, tc.want)
		}
		if err == nil {
			if _, err := Script(got); err != nil {
				t.Errorf("Script(%q) = %v", got, err)
			}
		}
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
	// A fake uname, for a run that takes the other OS's path (FAKE_UNAME_S, _M).
	fakeUname := filepath.Join(tmp, "fakeuname")
	if err := os.Mkdir(fakeUname, 0o755); err != nil {
		t.Fatal(err)
	}
	uname := "#!/bin/sh\ncase \"$1\" in -m) echo \"$FAKE_UNAME_M\" ;; *) echo \"$FAKE_UNAME_S\" ;; esac\n"
	if err := os.WriteFile(filepath.Join(fakeUname, "uname"), []byte(uname), 0o755); err != nil { //nolint:gosec // a test's fake tool
		t.Fatal(err)
	}
	// runWith runs script with args in a temp HOME of its own (so it never
	// reads this computer's service files): o.saved is a token the worker's data
	// dir already holds ("" for none: a first install), o.noDir leaves --dir
	// out, o.goos fakes uname (darwin or linux), o.service writes a service
	// file into the HOME first.
	type runOpts struct {
		saved, goos string
		noDir       bool
		service     func(home string)
	}
	type result struct {
		out, ran, url, home string
		err                 error
	}
	runWith := func(t *testing.T, script string, o runOpts, args ...string) result {
		t.Helper()
		sub := t.TempDir()
		home := filepath.Join(sub, "home")
		if err := os.Mkdir(home, 0o755); err != nil {
			t.Fatal(err)
		}
		ran, urls, data := filepath.Join(sub, "ran"), filepath.Join(sub, "url"), filepath.Join(sub, "data")
		if o.saved != "" {
			if err := os.MkdirAll(data, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "worker.token"), []byte(o.saved+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if o.service != nil {
			o.service(home)
		}
		argv := []string{"-s", "--"}
		if !o.noDir {
			argv = append(argv, "--dir", filepath.Join(sub, "bin"))
		}
		path := fakeBin + ":" + os.Getenv("PATH")
		env := []string{"SERVED=" + served, "RAN=" + ran, "URLS=" + urls, "HOME=" + home,
			"AUDIOSILO_WORKER_DATA=" + data, "AUDIOSILO_WORKER_TOKEN="}
		switch o.goos {
		case "darwin":
			path = fakeUname + ":" + path
			env = append(env, "FAKE_UNAME_S=Darwin", "FAKE_UNAME_M=arm64")
		case "linux":
			path = fakeUname + ":" + path
			env = append(env, "FAKE_UNAME_S=Linux", "FAKE_UNAME_M=x86_64")
		}
		cmd := exec.Command("sh", append(argv, args...)...)
		cmd.Stdin = strings.NewReader(script)
		cmd.Env = append(append(os.Environ(), "PATH="+path), env...)
		out, err := cmd.CombinedOutput()
		r, _ := os.ReadFile(ran)
		u, _ := os.ReadFile(urls)
		if err == nil && !o.noDir {
			if fi, serr := os.Stat(filepath.Join(sub, "bin", "audiosilo-bridge")); serr != nil || fi.Mode().Perm() != 0o755 {
				t.Fatalf("the program = %v, %v", fi, serr)
			}
		}
		return result{string(out), strings.TrimSpace(string(r)), strings.TrimSpace(string(u)), home, err}
	}
	run := func(t *testing.T, script, saved string, args ...string) (string, string, string, error) {
		t.Helper()
		r := runWith(t, script, runOpts{saved: saved}, args...)
		return r.out, r.ran, r.url, r.err
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
		if !strings.Contains(out, "A --run start does not save the token") {
			t.Fatalf("--run doesn't say the token isn't saved:\n%s", out)
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
	// An update with no --dir goes where the installed service runs the
	// program from (the service file as `worker install` writes it), and says
	// so; an explicit --dir wins; a service file that doesn't name an existing
	// absolute folder plainly leaves the default.
	for _, goos := range []string{"darwin", "linux"} {
		// service writes goos's service file running bin (made a folder when
		// mkdir is set).
		service := func(bin string, mkdir bool) func(home string) {
			return func(home string) {
				svc := worker.Service{GOOS: goos, Home: home, Binary: bin, DataDir: filepath.Join(home, ".audiosilo-bridge-worker"),
					BridgeURL: "http://192.168.1.20:8091", Path: "/usr/bin:/bin"}
				text, err := svc.Render()
				if err != nil {
					t.Fatal(err)
				}
				file, err := svc.File()
				if err != nil {
					t.Fatal(err)
				}
				if mkdir {
					if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		custom := filepath.Join(tmp, goos+"-custom dir", "bin")
		t.Run(goos+"/reuse", func(t *testing.T) {
			r := runWith(t, text, runOpts{goos: goos, noDir: true, saved: "asw_saved",
				service: service(filepath.Join(custom, "audiosilo-bridge"), true)})
			if r.err != nil || !strings.Contains(r.out, "Updating the worker in "+custom+", where its service runs it from") ||
				!strings.Contains(r.out, "Installed audiosilo-bridge v9.9.9 at "+filepath.Join(custom, "audiosilo-bridge")) {
				t.Fatalf("reuse: %v\n%s", r.err, r.out)
			}
			if _, err := os.Stat(filepath.Join(r.home, ".audiosilo-bridge-worker", "bin")); err == nil {
				t.Fatal("the default folder was made")
			}
			if r.ran != "worker install --bridge http://192.168.1.20:8091" {
				t.Fatalf("ran %q", r.ran)
			}
		})
		t.Run(goos+"/explicit", func(t *testing.T) {
			other := filepath.Join(tmp, goos+"-other")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			r := runWith(t, text, runOpts{goos: goos, noDir: true, saved: "asw_saved",
				service: service(filepath.Join(other, "audiosilo-bridge"), true)}, "--dir", custom+"-explicit")
			if r.err != nil || strings.Contains(r.out, "Updating the worker in") ||
				!strings.Contains(r.out, "at "+filepath.Join(custom+"-explicit", "audiosilo-bridge")) {
				t.Fatalf("explicit --dir: %v\n%s", r.err, r.out)
			}
		})
		for name, bin := range map[string]string{
			"gone":     filepath.Join(tmp, goos+"-gone", "audiosilo-bridge"),
			"relative": "bin/audiosilo-bridge",
			"root":     "/audiosilo-bridge",
			"escaped":  filepath.Join(tmp, goos+`-a"b%c&d`, "audiosilo-bridge"),
			"dollar":   filepath.Join(tmp, goos+"-$HOME", "audiosilo-bridge"),
		} {
			t.Run(goos+"/"+name, func(t *testing.T) {
				r := runWith(t, text, runOpts{goos: goos, noDir: true, saved: "asw_saved", service: service(bin, name != "gone")})
				def := filepath.Join(r.home, ".audiosilo-bridge-worker", "bin", "audiosilo-bridge")
				if r.err != nil || strings.Contains(r.out, "Updating the worker in") || !strings.Contains(r.out, "at "+def) {
					t.Fatalf("%s: %v\n%s", bin, r.err, r.out)
				}
			})
		}
	}
	// A service file that isn't the one `worker install` writes, or holds
	// nothing usable, leaves the default.
	for name, tc := range map[string]struct{ goos, file, text string }{
		"plist no args":   {"darwin", "Library/LaunchAgents/app.audiosilo.bridge-worker.plist", "<plist><dict><key>Label</key><string>x</string></dict></plist>\n"},
		"plist garbage":   {"darwin", "Library/LaunchAgents/app.audiosilo.bridge-worker.plist", "\x00\xff not xml\n"},
		"unit no exec":    {"linux", ".config/systemd/user/audiosilo-bridge-worker.service", "[Service]\nRestart=always\n"},
		"unit prefixed":   {"linux", ".config/systemd/user/audiosilo-bridge-worker.service", "[Service]\nExecStart=-" + tmp + "/audiosilo-bridge worker\n"},
		"unit open quote": {"linux", ".config/systemd/user/audiosilo-bridge-worker.service", "[Service]\nExecStart=\"" + tmp + "/audiosilo-bridge\n"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runWith(t, text, runOpts{goos: tc.goos, noDir: true, saved: "asw_saved", service: func(home string) {
				f := filepath.Join(home, tc.file)
				if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, []byte(tc.text), 0o644); err != nil {
					t.Fatal(err)
				}
			}})
			def := filepath.Join(r.home, ".audiosilo-bridge-worker", "bin", "audiosilo-bridge")
			if r.err != nil || strings.Contains(r.out, "Updating the worker in") || !strings.Contains(r.out, "at "+def) {
				t.Fatalf("%v\n%s", r.err, r.out)
			}
		})
	}
	// A bare (unquoted) ExecStart, as a hand-written unit has it, is read too.
	t.Run("unit bare", func(t *testing.T) {
		bare := filepath.Join(tmp, "bare", "bin")
		if err := os.MkdirAll(bare, 0o755); err != nil {
			t.Fatal(err)
		}
		r := runWith(t, text, runOpts{goos: "linux", noDir: true, saved: "asw_saved", service: func(home string) {
			f := filepath.Join(home, ".config/systemd/user/audiosilo-bridge-worker.service")
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte("[Service]\n  ExecStart= "+bare+"/audiosilo-bridge worker --data x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}})
		if r.err != nil || !strings.Contains(r.out, "Updating the worker in "+bare+",") {
			t.Fatalf("%v\n%s", r.err, r.out)
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
