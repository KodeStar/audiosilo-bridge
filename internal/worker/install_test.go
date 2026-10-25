package worker

import (
	"encoding/xml"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func service(t *testing.T, goos string) (Service, *[]string) {
	t.Helper()
	var ran []string
	return Service{GOOS: goos, Home: t.TempDir(), UID: 501, Binary: "/Applications/Audio Silo/audiosilo-bridge",
		DataDir: "/Users/me/.audiosilo-bridge-worker", BridgeURL: "http://192.168.1.20:8091/?a=1&b=<2>",
		Args: []string{"--backend", "mlx-whisper"}, Path: "/opt/homebrew/bin:/usr/bin:/bin",
		Run: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			if len(args) > 0 && args[0] == "print" {
				return errNotLoaded // launchd doesn't know the job (it unloaded)
			}
			return nil
		},
		Sleep: func(time.Duration) {}}, &ran
}

// errNotLoaded is launchctl print on a job launchd doesn't know.
const errNotLoaded = exitError(113)

// exitError is what exec reports for a command that exited non-zero.
type exitError int

func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// The plist is well-formed XML whatever the paths hold, and runs the worker
// with the data dir, the Bridge and the extra flags, never the token.
func TestPlist(t *testing.T) {
	t.Parallel()
	s, _ := service(t, "darwin")
	text, err := s.Render()
	if err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.Strict = true
	var strs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("not well-formed: %v\n%s", err, text)
		}
		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}
	want := []string{LaunchdLabel, "/Applications/Audio Silo/audiosilo-bridge", "worker", "--data", "/Users/me/.audiosilo-bridge-worker",
		"--bridge", "http://192.168.1.20:8091/?a=1&b=<2>", "--backend", "mlx-whisper", "PATH", "/opt/homebrew/bin:/usr/bin:/bin"}
	// In this order, with nothing but keys between them.
	i := 0
	for _, got := range strs {
		if i < len(want) && got == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("plist strings = %q, want in order %q", strs, want)
	}
	for _, must := range []string{"<key>KeepAlive</key>", "<string>Interactive</string>", "/Users/me/.audiosilo-bridge-worker/worker.log"} {
		if !strings.Contains(text, must) {
			t.Errorf("plist lacks %s", must)
		}
	}
	if strings.Contains(text, "token") {
		t.Error("the plist mentions a token")
	}
}

// The unit quotes every word (spaces, quotes, backslashes, % specifiers, and
// ExecStart's $ variables).
func TestUnit(t *testing.T) {
	t.Parallel()
	s, _ := service(t, "linux")
	s.Binary = `/opt/a "b"\c/100%/$HOME/audiosilo-bridge`
	text, _ := s.Render()
	if !strings.Contains(text, `ExecStart="/opt/a \"b\"\\c/100%%/$$HOME/audiosilo-bridge" "worker" "--data" "/Users/me/.audiosilo-bridge-worker" "--bridge"`) {
		t.Fatalf("unit:\n%s", text)
	}
	for _, must := range []string{`Environment="PATH=/opt/homebrew/bin:/usr/bin:/bin"`, "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(text, must) {
			t.Errorf("unit lacks %s", must)
		}
	}
}

func TestInstallUninstall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		goos      string
		file      string
		install   []string
		uninstall []string
	}{
		{"darwin", "Library/LaunchAgents/app.audiosilo.bridge-worker.plist",
			[]string{"launchctl bootout gui/501/app.audiosilo.bridge-worker", "launchctl print gui/501/app.audiosilo.bridge-worker",
				"launchctl bootstrap gui/501 "},
			[]string{"launchctl bootout gui/501/app.audiosilo.bridge-worker"}},
		{"linux", ".config/systemd/user/audiosilo-bridge-worker.service",
			[]string{"systemctl --user daemon-reload", "systemctl --user enable --now audiosilo-bridge-worker.service",
				"systemctl --user restart audiosilo-bridge-worker.service"},
			[]string{"systemctl --user disable --now audiosilo-bridge-worker.service", "systemctl --user daemon-reload"}},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			t.Parallel()
			s, ran := service(t, tc.goos)
			path, err := s.Install()
			if err != nil || path != filepath.Join(s.Home, tc.file) {
				t.Fatalf("install = %s, %v", path, err)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
				t.Fatalf("the service file = %v, %v", fi, err)
			}
			if len(*ran) != len(tc.install) {
				t.Fatalf("ran %q", *ran)
			}
			for i, want := range tc.install {
				if !strings.HasPrefix((*ran)[i], want) {
					t.Fatalf("ran %q, want %q", *ran, tc.install)
				}
			}
			*ran = nil
			if _, err := s.Uninstall(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("the service file is still there")
			}
			if strings.Join(*ran, "|") != strings.Join(tc.uninstall, "|") {
				t.Fatalf("uninstall ran %q", *ran)
			}
		})
	}
}

// A reinstall over a running worker: launchctl bootout returns while the old
// job is still tearing down, so Install waits for it to go and retries the
// bootstrap that launchd refuses meanwhile (exit status 5).
func TestInstallWaitsForBootout(t *testing.T) {
	t.Parallel()
	s, _ := service(t, "darwin")
	var ran []string
	var slept time.Duration
	prints, bootstraps := 0, 0
	s.Sleep = func(d time.Duration) { slept += d }
	s.Run = func(name string, args ...string) error {
		ran = append(ran, args[0])
		switch args[0] {
		case "print":
			prints++
			if prints <= 2 {
				return nil // still loaded
			}
			return errNotLoaded
		case "bootstrap":
			bootstraps++
			if bootstraps == 1 {
				return exitError(5)
			}
		}
		return nil
	}
	if _, err := s.Install(); err != nil {
		t.Fatalf("install = %v", err)
	}
	if got := strings.Join(ran, " "); got != "bootout print print print bootstrap bootstrap" {
		t.Fatalf("ran %s", got)
	}
	if want := 2*bootoutPoll + bootstrapBackoff; slept != want {
		t.Fatalf("slept %v, want %v", slept, want)
	}
}

// A job that never unloads and a bootstrap that always fails: Install gives up
// in bounded time and says how to load the written file by hand.
func TestInstallFailure(t *testing.T) {
	t.Parallel()
	s, _ := service(t, "darwin")
	var slept time.Duration
	prints, bootstraps := 0, 0
	s.Sleep = func(d time.Duration) { slept += d }
	s.Run = func(name string, args ...string) error {
		switch args[0] {
		case "print":
			prints++
			return nil // never goes
		case "bootstrap":
			bootstraps++
			return exitError(5)
		}
		return nil
	}
	path, err := s.Install()
	if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap: exit status 5") ||
		!strings.Contains(err.Error(), "launchctl bootstrap gui/501 "+path) {
		t.Fatalf("install = %v", err)
	}
	if prints != int(bootoutWait/bootoutPoll) || bootstraps != bootstrapAttempts {
		t.Fatalf("print ran %d times, bootstrap %d", prints, bootstraps)
	}
	if limit := bootoutWait + 16*bootstrapBackoff; slept > limit {
		t.Fatalf("slept %v, more than %v", slept, limit)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the service file: %v", err)
	}
}

func TestUnsupported(t *testing.T) {
	t.Parallel()
	s, _ := service(t, "windows")
	if _, err := s.Install(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("install = %v", err)
	}
	if _, err := s.Render(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("render = %v", err)
	}
}

// Security: the token file is 0600, and a blank token isn't saved.
func TestTokenFile(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	if err := SaveToken(dir, "  "); err == nil {
		t.Fatal("a blank token was saved")
	}
	if LoadToken(dir) != "" {
		t.Fatal("a token from nowhere")
	}
	if err := SaveToken(dir, " asw_abc \n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, TokenFileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file = %v, %v", fi, err)
	}
	if got := LoadToken(dir); got != "asw_abc" {
		t.Fatalf("load = %q", got)
	}
}

// InstalledArgs reads back the command line Render wrote, whatever its words
// hold: XML's and systemd's special characters, spaces, quotes, and a Bridge
// address with every path character public_url may carry.
func TestInstalledArgsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			s, _ := service(t, goos)
			s.Binary = `/opt/a "b"\c/100%/$HOME/<x>&'y'/audiosilo-bridge`
			s.DataDir = "/Users/me/my data;%h$$/worker"
			s.BridgeURL = "https://proxy.lan:8443/a!$&()*+,;=:@b/c%20d%n"
			s.Args = []string{"--backend", "whisper-cpp", "--model", "large v3 \"q5\"", "--whisper-cli", `/opt/w\cli/$1/whisper-cli`}
			if _, err := s.Install(); err != nil {
				t.Fatal(err)
			}
			got, err := s.InstalledArgs()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(s.argv(), "\x00") {
				t.Fatalf("read back %q, want %q", got, s.argv())
			}
		})
	}
}

// The new address characters reach each service file safely: escaped in the
// plist's XML, quoted with % and $ doubled in the unit (so systemd expands
// neither, and ";" is no command separator).
func TestServiceFilesCarryTheAddress(t *testing.T) {
	t.Parallel()
	const addr = "http://proxy.lan/a!$&()*+,;=:@b%20"
	s, _ := service(t, "darwin")
	s.BridgeURL = addr
	text, _ := s.Render()
	if !strings.Contains(text, "<string>http://proxy.lan/a!$&amp;()*+,;=:@b%20</string>") {
		t.Fatalf("plist:\n%s", text)
	}
	s.GOOS = "linux"
	text, _ = s.Render()
	if !strings.Contains(text, `"--bridge" "http://proxy.lan/a!$$&()*+,;=:@b%%20"`) {
		t.Fatalf("unit:\n%s", text)
	}
}

// No service file is fs.ErrNotExist (a first install); one that doesn't read
// as Render writes it is an error, so the caller keeps its defaults.
func TestInstalledArgsMalformed(t *testing.T) {
	t.Parallel()
	s, _ := service(t, "darwin")
	if _, err := s.InstalledArgs(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no file = %v", err)
	}
	for _, tc := range []struct{ goos, text string }{
		{"darwin", ""},
		{"darwin", "\x00\xff not xml\n"},
		{"darwin", "<plist><dict><key>Label</key><string>x</string></dict></plist>\n"},
		{"darwin", "<plist><dict><key>ProgramArguments</key><array><string>/a</string>"},
		{"darwin", "<plist><dict><key>ProgramArguments</key><array><string>/a</string><integer>1</integer></array></dict></plist>"},
		{"darwin", "<plist><dict><key>ProgramArguments</key><array></array></dict></plist>"},
		{"darwin", "<plist><dict><key>ProgramArguments</key><string>/a</string><array><string>/b</string></array></dict></plist>"},
		{"linux", "[Service]\nRestart=always\n"},
		{"linux", "[Service]\nExecStart=\n"},
		{"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\n"},
		{"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\"x worker\n"},
		{"linux", "[Service]\nExecStart=-/a/audiosilo-bridge worker\n"},
		{"linux", "[Service]\nExecStart=\"bin/audiosilo-bridge\" worker\n"},
		{"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\" \"worker\" \"--data\" \"%h/w\"\n"},
		{"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\" \"worker\" \"--data\" \"$HOME/w\"\n"},
		{"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\" \"worker\" \"--data\" \"/w\\n\"\n"},
		{"linux", "[Service]\nExecStart=/a/audiosilo-bridge worker --data /w\\\n  --model x\n"},
		{"linux", "[Service]\nExecStart=/a/audiosilo-bridge worker --data /w%\n"},
	} {
		s, _ := service(t, tc.goos)
		writeServiceFile(t, s, tc.text)
		if got, err := s.InstalledArgs(); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s %q = %q, %v", tc.goos, tc.text, got, err)
		}
	}
	// A bare ExecStart, as a hand-written unit has it, reads.
	s, _ = service(t, "linux")
	writeServiceFile(t, s, "[Service]\n  ExecStart= /a/audiosilo-bridge worker  --data /w%%x\t--bridge \"http://h:1\"\n")
	got, err := s.InstalledArgs()
	if err != nil || strings.Join(got, " ") != "/a/audiosilo-bridge worker --data /w%x --bridge http://h:1" {
		t.Fatalf("bare = %q, %v", got, err)
	}
}

// writeServiceFile writes text as s's service file.
func writeServiceFile(t *testing.T, s Service, text string) {
	t.Helper()
	path, err := s.File()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
