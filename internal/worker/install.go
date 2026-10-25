package worker

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// worker install / uninstall: a launchd agent on macOS (the user's own session,
// where Metal is) or a systemd user unit on Linux, so the worker always runs. The
// token goes into <data>/worker.token (0600), never into the service file.

// LaunchdLabel and SystemdUnit name the service.
const (
	LaunchdLabel = "app.audiosilo.bridge-worker"
	SystemdUnit  = "audiosilo-bridge-worker.service"
)

// TokenFileName is the token's file in the data dir.
const TokenFileName = "worker.token"

// LogFileName is the service's log in the data dir (launchd; systemd logs to
// the journal).
const LogFileName = "worker.log"

// Service describes the installed worker.
type Service struct {
	// GOOS picks launchd (darwin) or systemd (linux).
	GOOS string
	// Home is the user's home directory; UID the user's id (launchctl's
	// gui/<uid> domain).
	Home string
	UID  int
	// Binary is this executable's absolute path; DataDir the worker's data dir.
	Binary  string
	DataDir string
	// BridgeURL is the Bridge's page; Args are more worker flags (--backend).
	BridgeURL string
	Args      []string
	// Path is the PATH the service runs with: launchd's default lacks
	// Homebrew, where python3 and ffmpeg (mlx-whisper's) live.
	Path string
	// Run runs a service manager command (launchctl, systemctl).
	Run func(name string, args ...string) error
	// Sleep waits between launchctl checks and retries (nil: time.Sleep).
	Sleep func(time.Duration)
}

// How long Install waits for launchd to unload the previous worker, and how
// often it retries a bootstrap. launchctl bootout returns before the old job
// has finished tearing down, and a bootstrap meanwhile fails with exit status
// 5 (Input/output error).
const (
	bootoutWait       = 10 * time.Second
	bootoutPoll       = 250 * time.Millisecond
	bootstrapAttempts = 5
	bootstrapBackoff  = 500 * time.Millisecond
)

// ErrUnsupported: there is no service manager this knows on the platform.
var ErrUnsupported = errors.New("worker install supports macOS (launchd) and Linux (systemd)")

// File is where the service file goes.
func (s Service) File() (string, error) {
	switch s.GOOS {
	case "darwin":
		return filepath.Join(s.Home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
	case "linux":
		return filepath.Join(s.Home, ".config", "systemd", "user", SystemdUnit), nil
	}
	return "", ErrUnsupported
}

// argv is the service's command line.
func (s Service) argv() []string {
	return append([]string{s.Binary, "worker", "--data", s.DataDir, "--bridge", s.BridgeURL}, s.Args...)
}

// Render is the service file's text.
func (s Service) Render() (string, error) {
	switch s.GOOS {
	case "darwin":
		return s.plist(), nil
	case "linux":
		return s.unit(), nil
	}
	return "", ErrUnsupported
}

// plist is the launchd agent. KeepAlive restarts the worker whenever it stops;
// launchd also ends what is left of its process group (a decode it started),
// so a worker that dies never leaves a decoder holding the GPU. ProcessType
// Interactive keeps launchd from throttling it like a background job.
func (s Service) plist() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + LaunchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range s.argv() {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	log := filepath.Join(s.DataDir, LogFileName)
	b.WriteString(`	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + xmlEscape(s.Path) + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// unit is the systemd user unit. Restart=always brings it back; stopping it
// ends its whole cgroup, a running decode included.
func (s Service) unit() string {
	args := make([]string, 0, len(s.argv()))
	for _, a := range s.argv() {
		// ExecStart= also expands $VAR and ${VAR} (Environment= doesn't): $$ is a
		// literal dollar.
		args = append(args, systemdQuote(strings.ReplaceAll(a, "$", "$$")))
	}
	return `[Unit]
Description=AudioSilo Bridge ASR worker
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + strings.Join(args, " ") + `
Environment=` + systemdQuote("PATH="+s.Path) + `
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes one word of a unit's command line: double quotes, with
// backslash and quote escaped and % doubled (systemd's specifier character).
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "\n", " ").Replace(s)
	return `"` + s + `"`
}

// InstalledArgs is the command line of the installed service, read back from
// its file (the plist's ProgramArguments, the unit's first ExecStart): the
// inverse of Render, so an update can keep the flags the service runs with.
// No file is an error wrapping fs.ErrNotExist (a first install); a file that
// doesn't read as one Render writes is an error too (the caller keeps its
// defaults then): unreadable, not well-formed, or holding something systemd
// would expand (a specifier, a $VAR) whose value isn't known here.
func (s Service) InstalledArgs() ([]string, error) {
	path, err := s.File()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) //nolint:gosec // the user's own service file, where File puts it
	if err != nil {
		return nil, err
	}
	var argv []string
	if s.GOOS == "darwin" {
		argv, err = plistArgs(b)
	} else {
		argv, err = unitArgs(string(b))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: no command line", path)
	}
	return argv, nil
}

// plistArgs is the strings of the ProgramArguments array.
func plistArgs(b []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	lastKey := ""
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no ProgramArguments")
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case se.Name.Local == "key":
			if err := dec.DecodeElement(&lastKey, &se); err != nil {
				return nil, err
			}
			continue
		case se.Name.Local == "array" && lastKey == "ProgramArguments":
			var arr struct {
				Strings []string   `xml:"string"`
				Other   []xml.Name `xml:",any"`
			}
			if err := dec.DecodeElement(&arr, &se); err != nil {
				return nil, err
			}
			if len(arr.Other) > 0 {
				return nil, errors.New("ProgramArguments holds something other than strings")
			}
			return arr.Strings, nil
		}
		lastKey = ""
	}
}

// unitArgs is the first ExecStart's words.
func unitArgs(text string) ([]string, error) {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			return splitExec(v)
		}
	}
	return nil, errors.New("no ExecStart")
}

// splitExec splits an ExecStart command line the way unit writes one: words
// apart by blanks, each "quoted" (with \\ and \" its only escapes) or bare,
// %% and $$ a literal % and $. Anything else (another backslash escape, a
// specifier, a $VAR, an open quote, a single quote in a bare word, which
// systemd unquotes, a lone ; it splits commands at, a prefix such as - or @
// on the program) means systemd reads it in a way this doesn't, so it is
// refused.
func splitExec(v string) ([]string, error) {
	var words []string
	blank := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' }
	for i := 0; ; {
		for i < len(v) && blank(v[i]) {
			i++
		}
		if i == len(v) {
			break
		}
		var w strings.Builder
		if v[i] == '"' {
			closed := false
			for i++; i < len(v); i++ {
				c := v[i]
				if c == '"' {
					closed = true
					i++
					break
				}
				if c == '\\' {
					if i+1 == len(v) || (v[i+1] != '\\' && v[i+1] != '"') {
						return nil, errors.New("ExecStart has an escape this doesn't read")
					}
					i++
					c = v[i]
				}
				w.WriteByte(c)
			}
			if !closed || (i < len(v) && !blank(v[i])) {
				return nil, errors.New("ExecStart has a quote this doesn't read")
			}
		} else {
			for ; i < len(v) && !blank(v[i]); i++ {
				if v[i] == '"' || v[i] == '\'' || v[i] == '\\' {
					return nil, errors.New("ExecStart has a quote or escape this doesn't read")
				}
				w.WriteByte(v[i])
			}
			if w.String() == ";" {
				return nil, errors.New("ExecStart has more than one command")
			}
		}
		word, err := unexpand(w.String())
		if err != nil {
			return nil, err
		}
		words = append(words, word)
	}
	if len(words) > 0 && !strings.HasPrefix(words[0], "/") {
		return nil, errors.New("ExecStart's program isn't an absolute path")
	}
	return words, nil
}

// unexpand turns a unit word's %% and $$ back into % and $; any other % or $
// is a specifier or a variable systemd expands, refused.
func unexpand(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' || c == '$' {
			if i+1 == len(s) || s[i+1] != c {
				return "", fmt.Errorf("ExecStart has a %c that systemd expands", c)
			}
			i++
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

// SaveToken writes the worker token into the data dir (0600).
func SaveToken(dataDir, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("no token: make one on the Bridge's page (Workers)")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, TokenFileName), []byte(token+"\n"), 0o600)
}

// LoadToken reads the token from the data dir ("" when there is none).
func LoadToken(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, TokenFileName)) //nolint:gosec // the worker's own data dir
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Install writes the service file and starts the service.
func (s Service) Install() (string, error) {
	path, err := s.File()
	if err != nil {
		return "", err
	}
	text, err := s.Render()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the user's own service dir
		return "", err
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil { //nolint:gosec // a service file the manager reads; no secret in it
		return "", err
	}
	switch s.GOOS {
	case "darwin":
		if err := s.bootstrap(path); err != nil {
			return path, err
		}
	case "linux":
		if err := s.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return path, fmt.Errorf("systemctl --user daemon-reload: %w", err)
		}
		if err := s.Run("systemctl", "--user", "enable", "--now", SystemdUnit); err != nil {
			return path, fmt.Errorf("systemctl --user enable --now: %w", err)
		}
		_ = s.Run("systemctl", "--user", "restart", SystemdUnit) // an update picks up the new file
	}
	return path, nil
}

// bootstrap (re)loads the launchd agent: it unloads the old one, waits until
// launchd no longer knows it (bounded), then bootstraps the new file, retrying
// with a growing pause while launchd is still busy with the old job.
func (s Service) bootstrap(path string) error {
	sleep := s.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	domain := "gui/" + strconv.Itoa(s.UID)
	target := domain + "/" + LaunchdLabel
	_ = s.Run("launchctl", "bootout", target) // not loaded yet is fine
	for waited := time.Duration(0); waited < bootoutWait; waited += bootoutPoll {
		if s.Run("launchctl", "print", target) != nil {
			break // gone
		}
		sleep(bootoutPoll)
	}
	var err error
	pause := bootstrapBackoff
	for attempt := 1; attempt <= bootstrapAttempts; attempt++ {
		if err = s.Run("launchctl", "bootstrap", domain, path); err == nil {
			return nil
		}
		if attempt < bootstrapAttempts {
			sleep(pause)
			pause *= 2
		}
	}
	return fmt.Errorf("launchctl bootstrap: %w (the service file is written: try again in a moment with `launchctl bootstrap %s %s`)",
		err, domain, path)
}

// Uninstall stops the service and removes its file (the data dir stays).
func (s Service) Uninstall() (string, error) {
	path, err := s.File()
	if err != nil {
		return "", err
	}
	switch s.GOOS {
	case "darwin":
		_ = s.Run("launchctl", "bootout", "gui/"+strconv.Itoa(s.UID)+"/"+LaunchdLabel)
	case "linux":
		_ = s.Run("systemctl", "--user", "disable", "--now", SystemdUnit)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return path, err
	}
	if s.GOOS == "linux" {
		_ = s.Run("systemctl", "--user", "daemon-reload")
	}
	return path, nil
}
