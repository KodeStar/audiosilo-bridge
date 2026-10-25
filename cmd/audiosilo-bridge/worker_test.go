package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/worker"
)

// installedService writes goos's service file into home as `worker install`
// renders it, running with args besides --data and --bridge.
func installedService(t *testing.T, goos, home, data, bridge string, args ...string) worker.Service {
	t.Helper()
	svc := worker.Service{GOOS: goos, Home: home, Binary: "/opt/old/audiosilo-bridge", DataDir: data, BridgeURL: bridge,
		Args: args, Path: "/usr/bin:/bin"}
	text, err := svc.Render()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, svc, text)
	return svc
}

func writeFile(t *testing.T, svc worker.Service, text string) {
	t.Helper()
	path, err := svc.File()
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

// parsedFlags is `worker install`'s flags parsed from args.
func parsedFlags(t *testing.T, args ...string) (*flag.FlagSet, workerFlags) {
	t.Helper()
	fs := flag.NewFlagSet("worker install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := addWorkerFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs, f
}

// An update keeps the installed service's flags (round-trip: read back with
// the flags that wrote them), an explicit flag wins, and a service file that
// doesn't read as `worker install` writes it keeps nothing.
func TestCarryForward(t *testing.T) {
	t.Parallel()
	const bridge = "http://proxy.lan/my+bridge(1)!$&*,;=:@x"
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			data := filepath.Join(home, "my data $x%y")
			svc := installedService(t, goos, home, data, bridge,
				"--backend", "whisper-cpp", "--model", "large-v3 q5", "--whisper-cli", "/opt/w cli/whisper-cli")

			// Nothing given: everything kept (named in flag order).
			fs, f := parsedFlags(t)
			kept, err := carryForward(fs, svc)
			if err != nil || strings.Join(kept, " ") != "--backend --bridge --data --model --whisper-cli" {
				t.Fatalf("kept %q, %v", kept, err)
			}
			if *f.data != data || *f.bridge != bridge || *f.backend != "whisper-cpp" || *f.model != "large-v3 q5" ||
				*f.whisperCLI != "/opt/w cli/whisper-cli" || *f.token != "" {
				t.Fatalf("flags = %q %q %q %q %q", *f.data, *f.bridge, *f.backend, *f.model, *f.whisperCLI)
			}
			if got := f.passthrough(); strings.Join(got, "|") != "--backend|whisper-cpp|--model|large-v3 q5|--whisper-cli|/opt/w cli/whisper-cli" {
				t.Fatalf("passthrough = %q", got)
			}

			// Explicit flags win, an empty one included (clearing --model).
			fs, f = parsedFlags(t, "--bridge", "http://10.0.0.2:8091", "--backend", "auto", "--model", "", "--data", "/new")
			kept, err = carryForward(fs, svc)
			if err != nil || strings.Join(kept, " ") != "--whisper-cli" {
				t.Fatalf("kept %q, %v", kept, err)
			}
			if *f.data != "/new" || *f.bridge != "http://10.0.0.2:8091" || *f.backend != "auto" || *f.model != "" ||
				*f.whisperCLI != "/opt/w cli/whisper-cli" {
				t.Fatalf("flags = %q %q %q %q %q", *f.data, *f.bridge, *f.backend, *f.model, *f.whisperCLI)
			}
		})
	}
	// No service: a first install keeps nothing and says nothing.
	fs, f := parsedFlags(t)
	def := *f.data
	if kept, err := carryForward(fs, worker.Service{GOOS: "linux", Home: t.TempDir()}); err != nil || kept != nil || *f.data != def {
		t.Fatalf("no service: %q, %v", kept, err)
	}
	// No service manager: Install says so.
	if kept, err := carryForward(fs, worker.Service{GOOS: "windows", Home: t.TempDir()}); err != nil || kept != nil {
		t.Fatalf("windows: %q, %v", kept, err)
	}
	// Malformed: nothing kept, an error.
	for name, tc := range map[string]struct{ goos, text string }{
		"plist garbage":  {"darwin", "\x00\xff not xml\n"},
		"unit no exec":   {"linux", "[Service]\nRestart=always\n"},
		"unit specifier": {"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\" \"worker\" \"--data\" \"%h/w\"\n"},
		"not a worker":   {"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\" \"serve\" \"--data\" \"/w\"\n"},
		"unknown flag":   {"linux", "[Service]\nExecStart=\"/a/audiosilo-bridge\" \"worker\" \"--data\" \"/w\" \"--gpu\" \"1\"\n"},
		"stray word":     {"darwin", plist("/a/audiosilo-bridge", "worker", "--data", "/w", "extra")},
		"program only":   {"darwin", plist("/a/audiosilo-bridge")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := worker.Service{GOOS: tc.goos, Home: t.TempDir()}
			writeFile(t, svc, tc.text)
			fs, f := parsedFlags(t, "--backend", "mlx-whisper")
			def := *f.data
			kept, err := carryForward(fs, svc)
			if err == nil || kept != nil || *f.data != def || *f.backend != "mlx-whisper" {
				t.Fatalf("kept %q, %v, data %q", kept, err, *f.data)
			}
		})
	}
}

// plist is a launchd agent running argv.
func plist(argv ...string) string {
	var b strings.Builder
	b.WriteString("<plist version=\"1.0\"><dict><key>ProgramArguments</key><array>")
	for _, a := range argv {
		b.WriteString("<string>" + a + "</string>")
	}
	b.WriteString("</array></dict></plist>\n")
	return b.String()
}

// worker install --print on this computer's service manager, in a temp HOME:
// an update renders the installed service's flags, and says nothing about
// keeping them in the printed file; a malformed file says so on stderr and
// renders the defaults. Never runs launchctl or systemctl (--print).
func TestWorkerInstallPrintKeepsTheService(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("worker install supports macOS and Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(workerDataEnv, "")
	data := filepath.Join(home, "custom data")
	svc := installedService(t, runtime.GOOS, home, data, "http://10.0.0.5:8091", "--backend", "whisper-cpp", "--model", "small")
	var out, errb bytes.Buffer
	if err := workerCmd([]string{"install", "--print"}, &out, &errb); err != nil {
		t.Fatalf("%v: %s", err, errb.String())
	}
	argv := []string{"worker", "--data", data, "--bridge", "http://10.0.0.5:8091", "--backend", "whisper-cpp", "--model", "small"}
	for _, a := range argv {
		if !strings.Contains(out.String(), a) {
			t.Fatalf("printed service lacks %q:\n%s", a, out.String())
		}
	}
	if strings.Contains(out.String(), "Keeping") || errb.Len() != 0 {
		t.Fatalf("stdout:\n%s\nstderr: %s", out.String(), errb.String())
	}
	// An explicit --bridge and --backend win; the rest stays.
	out.Reset()
	if err := workerCmd([]string{"install", "--print", "--bridge", "http://10.0.0.9:8091", "--backend", "auto"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "http://10.0.0.9:8091") || strings.Contains(out.String(), "10.0.0.5") ||
		strings.Contains(out.String(), "whisper-cpp") || !strings.Contains(out.String(), "small") || !strings.Contains(out.String(), data) {
		t.Fatalf("explicit flags:\n%s", out.String())
	}
	// A malformed file: the defaults, and a word on stderr.
	writeFile(t, svc, "not a service file\n")
	out.Reset()
	err := workerCmd([]string{"install", "--print"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "--bridge is required") {
		t.Fatalf("malformed without --bridge = %v", err)
	}
	if err := workerCmd([]string{"install", "--print", "--bridge", "http://10.0.0.5:8091"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "custom data") || strings.Contains(out.String(), "whisper-cpp") ||
		!strings.Contains(out.String(), filepath.Join(home, ".audiosilo-bridge-worker")) ||
		!strings.Contains(errb.String(), "the installed service's settings can't be read") {
		t.Fatalf("malformed:\nstdout %s\nstderr %s", out.String(), errb.String())
	}
}
