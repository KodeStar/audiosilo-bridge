package worker

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func service(t *testing.T, goos string) (Service, *[]string) {
	t.Helper()
	var ran []string
	return Service{GOOS: goos, Home: t.TempDir(), UID: 501, Binary: "/Applications/Audio Silo/audiosilo-bridge",
		DataDir: "/Users/me/.audiosilo-bridge-worker", BridgeURL: "http://192.168.1.20:8091/?a=1&b=<2>",
		Args: []string{"--backend", "mlx-whisper"}, Path: "/opt/homebrew/bin:/usr/bin:/bin",
		Run: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return nil
		}}, &ran
}

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
			[]string{"launchctl bootout gui/501/app.audiosilo.bridge-worker", "launchctl bootstrap gui/501 "},
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

func TestInstallFailure(t *testing.T) {
	t.Parallel()
	s, _ := service(t, "darwin")
	s.Run = func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "bootstrap" {
			return errors.New("exit status 5")
		}
		return nil
	}
	if _, err := s.Install(); err == nil || !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Fatalf("install = %v", err)
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
