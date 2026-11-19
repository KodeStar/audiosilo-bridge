package agentproto

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeRequestAccepts(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"protocol":1,"cli":"claude","model":"","effort":"","web":false,"no_tools":false,"max_turns":200,"timeout_seconds":3600}`,
		`{"protocol":1,"cli":"codex","model":"gpt-5.1-codex","effort":"xhigh","web":true,"max_turns":0,"timeout_seconds":1}`,
		`{"protocol":1,"cli":"claude","model":"claude-opus-4-1[1m]","timeout_seconds":86400}`,
		`{"protocol":1,"cli":"codex","model":"qwen3:32b","timeout_seconds":60,"endpoint":{"base_url":"http://192.168.1.20:11434/v1"}}`,
		`{"protocol":1,"cli":"codex","model":"openai/gpt-oss-120b","timeout_seconds":60,"endpoint":{"base_url":"https://openrouter.ai/api/v1"}}`,
	} {
		if _, err := DecodeRequest([]byte(body)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestDecodeRequestRefuses(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"unknown field":       `{"protocol":1,"cli":"claude","timeout_seconds":60,"argv":["--dangerously-skip-permissions"]}`,
		"other protocol":      `{"protocol":2,"cli":"claude","timeout_seconds":60}`,
		"unknown cli":         `{"protocol":1,"cli":"bash","timeout_seconds":60}`,
		"no cli":              `{"protocol":1,"timeout_seconds":60}`,
		"model is a flag":     `{"protocol":1,"cli":"claude","model":"--tools","timeout_seconds":60}`,
		"model with a space":  `{"protocol":1,"cli":"claude","model":"opus x","timeout_seconds":60}`,
		"effort injection":    `{"protocol":1,"cli":"codex","effort":"high\",sandbox_mode=\"x","timeout_seconds":60}`,
		"turns negative":      `{"protocol":1,"cli":"claude","max_turns":-1,"timeout_seconds":60}`,
		"turns too many":      `{"protocol":1,"cli":"claude","max_turns":100000,"timeout_seconds":60}`,
		"no timeout":          `{"protocol":1,"cli":"claude"}`,
		"timeout too long":    `{"protocol":1,"cli":"claude","timeout_seconds":86401}`,
		"trailing data":       `{"protocol":1,"cli":"claude","timeout_seconds":60}{}`,
		"not json":            `cli=claude`,
		"wrong type":          `{"protocol":"1","cli":"claude","timeout_seconds":60}`,
		"oversized":           `{"protocol":1,"cli":"claude","timeout_seconds":60,"model":"` + strings.Repeat("a", MaxRequestBytes) + `"}`,
		"endpoint for claude": `{"protocol":1,"cli":"claude","model":"m","timeout_seconds":60,"endpoint":{"base_url":"http://h/v1"}}`,
		"endpoint no model":   `{"protocol":1,"cli":"codex","timeout_seconds":60,"endpoint":{"base_url":"http://h/v1"}}`,
		"endpoint bad url":    `{"protocol":1,"cli":"codex","model":"m","timeout_seconds":60,"endpoint":{"base_url":"file:///etc/passwd"}}`,
		"endpoint toml quote": `{"protocol":1,"cli":"codex","model":"m","timeout_seconds":60,"endpoint":{"base_url":"http://h/v1\",x=\"y"}}`,
		"endpoint extra":      `{"protocol":1,"cli":"codex","model":"m","timeout_seconds":60,"endpoint":{"base_url":"http://h/v1","key":"k"}}`,
	} {
		if _, err := DecodeRequest([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestJobID(t *testing.T) {
	t.Parallel()
	id, err := NewJobID()
	if err != nil || !ValidJobID(id) {
		t.Fatalf("NewJobID = %q, %v", id, err)
	}
	for _, bad := range []string{"", "runner.json", "0123456789ABCDEF", "0123456789abcde", "0123456789abcdef0", "../0123456789abc"} {
		if ValidJobID(bad) {
			t.Errorf("ValidJobID(%q) = true", bad)
		}
	}
}

func TestDecodeCredential(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ body, cli string }{
		{`{"cli":"claude","kind":"oauth_token","value":"sk-ant-oat01-abc"}`, CLIClaude},
		{`{"cli":"claude","kind":"api_key","value":"sk-ant-api03-abc"}`, CLIClaude},
		{`{"cli":"codex","kind":"api_key","value":"sk-proj-abc"}`, CLICodex},
		{`{"cli":"codex","kind":"endpoint_api_key","value":"sk-or-v1-abc"}`, CLICodex},
	} {
		c, err := DecodeCredential([]byte(tc.body), tc.cli)
		if err != nil || c.Value == "" {
			t.Errorf("%s: %+v %v", tc.body, c, err)
		}
	}
}

func TestDecodeCredentialRefuses(t *testing.T) {
	t.Parallel()
	const secret = "sk-SECRET-canary"
	for name, tc := range map[string]struct{ body, cli string }{
		"unknown field":      {`{"cli":"claude","kind":"api_key","value":"` + secret + `","env":"X=1"}`, CLIClaude},
		"another cli":        {`{"cli":"codex","kind":"api_key","value":"` + secret + `"}`, CLIClaude},
		"codex oauth":        {`{"cli":"codex","kind":"oauth_token","value":"` + secret + `"}`, CLICodex},
		"login kind":         {`{"cli":"codex","kind":"login","value":"` + secret + `"}`, CLICodex},
		"claude endpoint":    {`{"cli":"claude","kind":"endpoint_api_key","value":"` + secret + `"}`, CLIClaude},
		"empty value":        {`{"cli":"claude","kind":"api_key","value":""}`, CLIClaude},
		"newline in value":   {`{"cli":"claude","kind":"api_key","value":"a\nb` + secret + `"}`, CLIClaude},
		"space in value":     {`{"cli":"claude","kind":"api_key","value":"a b` + secret + `"}`, CLIClaude},
		"trailing data":      {`{"cli":"claude","kind":"api_key","value":"` + secret + `"} {}`, CLIClaude},
		"not an object":      {`"` + secret + `"`, CLIClaude},
		"value over the cap": {`{"cli":"claude","kind":"api_key","value":"` + strings.Repeat("a", MaxCredentialValueBytes+1) + `"}`, CLIClaude},
		"file over the cap":  {strings.Repeat(" ", MaxCredentialFileBytes+1), CLIClaude},
	} {
		_, err := DecodeCredential([]byte(tc.body), tc.cli)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error carries the value: %v", name, err)
		}
	}
}

// The endpoint's address (bridge#84): an http(s) URL with a host, and nothing a
// key or a `-c` override could ride in.
func TestCheckBaseURL(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"http://192.168.1.20:11434/v1", "https://api.openai.com/v1", "http://[fd00::1]:1234/v1",
		"http://host.docker.internal:11434/v1", "https://openrouter.ai/api/v1"} {
		if err := CheckBaseURL(ok); err != nil {
			t.Errorf("CheckBaseURL(%q) = %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty":       "",
		"scheme":      "ftp://h/v1",
		"no scheme":   "192.168.1.20:11434/v1",
		"file":        "file:///etc/passwd",
		"no host":     "http:///v1",
		"user":        "http://user@h/v1",
		"password":    "https://user:sk-secret@h/v1",
		"query":       "http://h/v1?key=sk-secret",
		"empty query": "http://h/v1?",
		"fragment":    "http://h/v1#x",
		"empty frag":  "http://h/v1#",
		"space":       "http://h/v 1",
		"quote":       `http://h/v1",x="y`,
		"backslash":   `http://h/v1\`,
		"newline":     "http://h/v1\nx",
		"non-ascii":   "http://hé/v1",
		"too long":    "http://h/" + strings.Repeat("a", MaxBaseURLBytes),
	} {
		err := CheckBaseURL(bad)
		if err == nil {
			t.Errorf("%s: %q accepted", name, bad)
			continue
		}
		if strings.Contains(err.Error(), "sk-secret") {
			t.Errorf("%s: the error repeats the address's secret: %v", name, err)
		}
	}
}

// TrimFound keeps a found path's printable characters alone, cut to
// MaxFoundBytes on a character's edge.
func TestTrimFound(t *testing.T) {
	t.Parallel()
	if got := TrimFound("/mnt/x holds /mnt/x/.audiosilo-agent-home"); got != "/mnt/x holds /mnt/x/.audiosilo-agent-home" {
		t.Fatalf("TrimFound = %q", got)
	}
	if got := TrimFound("/mnt/a\nb\x1b[31m"); got != "/mnt/a?b?[31m" {
		t.Fatalf("control characters = %q", got)
	}
	long := strings.Repeat("é", MaxFoundBytes)
	got := TrimFound(long)
	if len(got) > MaxFoundBytes || !utf8.ValidString(got) || len(got) < MaxFoundBytes-1 {
		t.Fatalf("a long one = %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}
