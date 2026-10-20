// Package podcast implements the feed.json contract and pure podcast transforms.
package podcast

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var ErrInvalidFeedURL = errors.New("invalid_feed_url")

// NormalizeFeedURL preserves credential bytes, escaped paths and query order.
func NormalizeFeedURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > 2048 {
		return "", ErrInvalidFeedURL
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", ErrInvalidFeedURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "", ErrInvalidFeedURL
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", ErrInvalidFeedURL
	}
	port := u.Port()
	if u.Scheme == "http" && port == "80" || u.Scheme == "https" && port == "443" {
		port = ""
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" {
		u.Path = "/"
	}
	// URL.String re-encodes userinfo. Retain its original spelling verbatim.
	authority := s[strings.Index(s, "://")+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if at := strings.LastIndex(authority, "@"); u.User != nil && at >= 0 {
		user := authority[:at]
		u.User = nil
		out := u.String()
		out = strings.Replace(out, "://", "://"+user+"@", 1)
		if len(out) > 2048 {
			return "", ErrInvalidFeedURL
		}
		return out, nil
	}
	out := u.String()
	if len(out) > 2048 {
		return "", ErrInvalidFeedURL
	}
	return out, nil
}

func FeedURLHash(norm string) string {
	h := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(h[:])
}

func RedactURL(norm string) string {
	u, err := url.Parse(norm)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "[redacted]"
	}
	out := u.Scheme + "://" + u.Host
	if u.EscapedPath() != "/" && u.EscapedPath() != "" || u.RawQuery != "" || u.ForceQuery {
		out += "/..."
	}
	return out
}

var credentialSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}$`)
var hasLetter = regexp.MustCompile(`[A-Za-z]`)
var hasDigit = regexp.MustCompile(`[0-9]`)
var privateHosts = []string{"patreon.com", "supercast.com", "supercast.tech", "memberfulcontent.com", "supportingcast.fm", "glow.fm", "steadyhq.com"}

func IsPrivateFeedURL(norm string) bool {
	u, err := url.Parse(norm)
	if err != nil {
		return true
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, parent := range privateHosts {
		if host == parent || strings.HasSuffix(host, "."+parent) {
			return true
		}
	}
	if strings.HasSuffix(host, "substack.com") && strings.Contains(u.Path, "/private/") {
		return true
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if credentialSegment.MatchString(seg) && hasLetter.MatchString(seg) && hasDigit.MatchString(seg) {
			return true
		}
	}
	return false
}

var urlInText = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)

// RedactText removes credentials from URLs in text from untrusted peers.
func RedactText(s string) string { return urlInText.ReplaceAllStringFunc(s, RedactURL) }
