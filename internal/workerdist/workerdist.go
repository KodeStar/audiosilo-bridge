// Package workerdist serves the ASR worker to the computers that run it: the
// gzipped audiosilo-bridge builds the images carry for each platform a worker
// runs on, and install.sh, which downloads the right one from the Bridge and
// installs it as a service (or runs it). A leaf: standard library only.
//
// The builds are the Bridge's own version (the image builds them with the same
// VERSION), so a worker installed from its Bridge always speaks its protocol.
// They live in a read-only folder of the image, never embedded in the binary;
// a Bridge run without that folder (a native build) has none and says where
// the releases are.
package workerdist

import (
	_ "embed"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultDir is where the images put the worker builds.
const DefaultDir = "/usr/local/share/audiosilo-bridge/workers"

// ReleasesURL is where the worker builds are published for a manual download.
const ReleasesURL = "https://github.com/KodeStar/audiosilo-bridge/releases"

// Platform is one OS/architecture a worker runs on.
type Platform struct {
	OS, Arch string
}

// File is the platform's gzipped build in the folder.
func (p Platform) File() string { return "audiosilo-bridge_" + p.OS + "_" + p.Arch + ".gz" }

// Platforms is the fixed set of worker builds (Windows has no worker).
var Platforms = []Platform{
	{"darwin", "arm64"},
	{"darwin", "amd64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
}

var (
	// ErrUnknownPlatform: the OS/architecture isn't one a worker is built for.
	ErrUnknownPlatform = errors.New("no worker build for that platform: macOS or Linux, arm64 or amd64")
	// ErrMissing: this Bridge has no build for it (no worker folder: a native
	// run, not the image).
	ErrMissing = errors.New("this Bridge has no worker downloads (it isn't running from the Docker image): download audiosilo-bridge for the worker's computer from " + ReleasesURL)
	// ErrAddress: the Bridge's address can't go into the script.
	ErrAddress = errors.New("the Bridge's address isn't a plain http(s) address")
)

// Dist is a folder of worker builds.
type Dist struct {
	Dir string
}

// lookup finds the platform in the fixed set; the file name comes from the set,
// never from the request.
func lookup(goos, arch string) (Platform, bool) {
	for _, p := range Platforms {
		if p.OS == goos && p.Arch == arch {
			return p, true
		}
	}
	return Platform{}, false
}

// Open opens the gzipped build for goos/arch (ErrUnknownPlatform, ErrMissing).
// The caller closes it.
func (d Dist) Open(goos, arch string) (*os.File, os.FileInfo, error) {
	p, ok := lookup(goos, arch)
	if !ok {
		return nil, nil, ErrUnknownPlatform
	}
	if d.Dir == "" {
		return nil, nil, ErrMissing
	}
	f, err := os.Open(filepath.Join(d.Dir, p.File())) //nolint:gosec // a fixed name from Platforms in the image's folder
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrMissing
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = ErrMissing
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// Available reports whether the folder holds a build for any platform.
func (d Dist) Available() bool {
	if d.Dir == "" {
		return false
	}
	for _, p := range Platforms {
		if info, err := os.Stat(filepath.Join(d.Dir, p.File())); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

//go:embed install.sh
var script string

// scriptAddress is the placeholder install.sh carries for the address.
const scriptAddress = "'@@BRIDGE@@'"

// hostPattern is what a host (with its port) may hold: a name (a LAN's own
// names can carry an underscore), an IPv4 or a bracketed IPv6 address.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9._\-]+(:[0-9]+)?$|^\[[0-9A-Fa-f:.]+\](:[0-9]+)?$`)

// Address is the Bridge's address as a worker reaches it: the configured
// public address when it is a plain http(s) one, else scheme://host from the
// request the script came in on (ErrAddress when neither is).
func Address(publicURL, scheme, host string) (string, error) {
	if u, err := url.Parse(strings.TrimSpace(publicURL)); err == nil && publicURL != "" &&
		(u.Scheme == "http" || u.Scheme == "https") && hostPattern.MatchString(u.Host) &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && !strings.ContainsAny(u.Path, "'\"\\`$ \t\r\n") {
		return strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/"), nil
	}
	if (scheme == "http" || scheme == "https") && hostPattern.MatchString(host) {
		return scheme + "://" + host, nil
	}
	return "", ErrAddress
}

// Script is install.sh with the Bridge's address baked in. address must come
// from Address; it goes in single-quoted all the same. TestScript holds that
// install.sh carries the placeholder.
func Script(address string) (string, error) {
	if address == "" || strings.ContainsAny(address, "'\r\n") {
		return "", ErrAddress
	}
	return strings.Replace(script, scriptAddress, "'"+address+"'", 1), nil
}
