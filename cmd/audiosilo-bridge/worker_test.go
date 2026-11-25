package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/worker"
	"github.com/kodestar/audiosilo-bridge/internal/workerdist"
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

// noServiceManager puts a launchctl and a systemctl that only fail at the
// front of $PATH (in dir), so a `worker install` that goes on past
// --check-token or --print fails instead of reaching this computer's own
// service.
func noServiceManager(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"launchctl", "systemctl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	// A malformed file: the defaults, and a word on stderr (also when the
	// missing --bridge stops it, since that is why it is missing).
	writeFile(t, svc, "not a service file\n")
	out.Reset()
	errb.Reset()
	err := workerCmd([]string{"install", "--print"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "--bridge is required") ||
		!strings.Contains(errb.String(), "the installed service's settings can't be read") {
		t.Fatalf("malformed without --bridge = %v, stderr %q", err, errb.String())
	}
	errb.Reset()
	if err := workerCmd([]string{"install", "--print", "--bridge", "http://10.0.0.5:8091"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "custom data") || strings.Contains(out.String(), "whisper-cpp") ||
		!strings.Contains(out.String(), filepath.Join(home, ".audiosilo-bridge-worker")) ||
		!strings.Contains(errb.String(), "the installed service's settings can't be read") {
		t.Fatalf("malformed:\nstdout %s\nstderr %s", out.String(), errb.String())
	}
}

// Exercise the downloaded program's real token lookup. The final install is
// stubbed so the test never registers a service on the host.
func TestWorkerInstallScriptUpdate(t *testing.T) {
	if os.Getenv("BRIDGE_TEST_CHECK_TOKEN") == "1" {
		if err := workerCmd(flag.Args(), os.Stdout, os.Stderr); err != nil {
			t.Fatal(err)
		}
		return
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the installer runs on macOS and Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(workerDataEnv, "")
	t.Setenv(workerTokenEnv, "")
	data := filepath.Join(home, `escaped & "data" $5%\`)
	bin := filepath.Join(home, "old worker", "audiosilo-bridge")
	if err := os.Mkdir(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := worker.Service{GOOS: runtime.GOOS, Home: home, DataDir: data, Binary: bin, BridgeURL: "http://bridge"}
	body, err := svc.Render()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		// Also cover the --data=value form in a hand-edited unit.
		body = strings.Replace(body, `"--data" "`, `"--data=`, 1)
	}
	writeFile(t, svc, body)
	if runtime.GOOS == "darwin" {
		path, _ := svc.File()
		if out, err := exec.Command("/usr/bin/plutil", "-convert", "binary1", path).CombinedOutput(); err != nil {
			t.Fatalf("binary plist: %v: %s", err, out)
		}
	}
	script, err := workerdist.Script(svc.BridgeURL)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_EXE", exe)
	t.Setenv("BRIDGE_TEST_CHECK_TOKEN", "1")
	// curl -fsSL -o <path> <url> serves a worker that delegates its check to
	// this test binary. Leave the installed binary alone until the check passes.
	curl := `#!/bin/sh
gzip -c > "$3" <<'WORKER'
#!/bin/sh
case "$1" in
version) echo v1 ;;
worker)
  if [ "$5" = --check-token ]; then
    shift
    exec "$TEST_EXE" -test.run='^TestWorkerInstallScriptUpdate$' -- "$@"
  fi
  echo "installed via $0"
  ;;
esac
WORKER
`
	if err := os.WriteFile(filepath.Join(home, "curl"), []byte(curl), 0o700); err != nil {
		t.Fatal(err)
	}
	// The check runs the real `worker install`.
	noServiceManager(t, home)
	runScript := func(args ...string) (string, error) {
		cmd := exec.Command("sh", append([]string{"-s", "--"}, args...)...)
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if err := worker.SaveToken(defaultWorkerDataDir(), "asw_wrong_folder"); err != nil {
		t.Fatal(err)
	}
	if out, err := runScript(); err == nil || !strings.Contains(out, "--token is required") {
		t.Fatalf("token only in the default folder: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(bin); err != nil || string(got) != "old" {
		t.Fatalf("old binary after refusal: %q, %v", got, err)
	}
	if err := worker.SaveToken(data, "asw_custom"); err != nil {
		t.Fatal(err)
	}
	if out, err := runScript(); err != nil || !strings.Contains(out, "installed via "+bin) {
		t.Fatalf("token in escaped service data folder: %v\n%s", err, out)
	}
	// A --run start looks where `worker` does (the default), not in the
	// service's --data.
	if err := os.Remove(filepath.Join(defaultWorkerDataDir(), worker.TokenFileName)); err != nil {
		t.Fatal(err)
	}
	if out, err := runScript("--run"); err == nil || !strings.Contains(out, "--token is required") {
		t.Fatalf("--run with the token only in the service's data folder: %v\n%s", err, out)
	}
}

// The installer's token check names an unreadable service only when it
// refuses: one that passes leaves it to the install that follows.
func TestWorkerInstallCheckTokenUnreadableService(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("worker install supports macOS and Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(workerDataEnv, "")
	t.Setenv(workerTokenEnv, "")
	noServiceManager(t, home)
	writeFile(t, worker.Service{GOOS: runtime.GOOS, Home: home}, "not a service file")
	check := func() (string, error) {
		var errb bytes.Buffer
		err := workerCmd([]string{"install", "--bridge", "http://bridge", "--check-token"}, io.Discard, &errb)
		return errb.String(), err
	}
	if msg, err := check(); !errors.Is(err, errTokenRequired) || !strings.Contains(msg, "can't be read") {
		t.Fatalf("no token: %v, %q", err, msg)
	}
	if err := worker.SaveToken(defaultWorkerDataDir(), "asw_default"); err != nil {
		t.Fatal(err)
	}
	if msg, err := check(); err != nil || msg != "" {
		t.Fatalf("a saved token: %v, %q", err, msg)
	}
}

func TestWorkerInstallDataAndModelOverrides(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("worker install supports macOS and Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	envData := filepath.Join(home, "env")
	t.Setenv(workerDataEnv, envData)
	noServiceManager(t, home)
	installedService(t, runtime.GOOS, home, filepath.Join(home, "old"), "http://bridge", "--model", "old-model", "--backend", "whisper-cpp")
	for _, tc := range []struct {
		name        string
		flags       []string
		data, model string
	}{
		{"environment", nil, envData, "old-model"},
		{"explicit", []string{"--data", filepath.Join(home, "explicit")}, filepath.Join(home, "explicit"), "old-model"},
		{"reset", []string{"--reset-model"}, envData, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := workerCmd(append([]string{"install", "--print"}, tc.flags...), &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.data) || !strings.Contains(out.String(), "whisper-cpp") ||
				strings.Contains(out.String(), "--model") != (tc.model != "") || !strings.Contains(out.String(), tc.model) {
				t.Fatalf("rendered service:\n%s", out.String())
			}
		})
	}
	if err := workerCmd([]string{"install", "--print", "--reset-model", "--model", "small"}, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted both a model override and a reset")
	}
}
