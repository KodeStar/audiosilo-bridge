package agentproto

import (
	"strings"
	"testing"
)

func TestDecodeRequestAccepts(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"protocol":1,"cli":"claude","model":"","effort":"","web":false,"no_tools":false,"max_turns":200,"timeout_seconds":3600}`,
		`{"protocol":1,"cli":"codex","model":"gpt-5.1-codex","effort":"xhigh","web":true,"max_turns":0,"timeout_seconds":1}`,
		`{"protocol":1,"cli":"claude","model":"claude-opus-4-1[1m]","timeout_seconds":86400}`,
	} {
		if _, err := DecodeRequest([]byte(body)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestDecodeRequestRefuses(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"unknown field":      `{"protocol":1,"cli":"claude","timeout_seconds":60,"argv":["--dangerously-skip-permissions"]}`,
		"other protocol":     `{"protocol":2,"cli":"claude","timeout_seconds":60}`,
		"unknown cli":        `{"protocol":1,"cli":"bash","timeout_seconds":60}`,
		"no cli":             `{"protocol":1,"timeout_seconds":60}`,
		"model is a flag":    `{"protocol":1,"cli":"claude","model":"--tools","timeout_seconds":60}`,
		"model with a space": `{"protocol":1,"cli":"claude","model":"opus x","timeout_seconds":60}`,
		"effort injection":   `{"protocol":1,"cli":"codex","effort":"high\",sandbox_mode=\"x","timeout_seconds":60}`,
		"turns negative":     `{"protocol":1,"cli":"claude","max_turns":-1,"timeout_seconds":60}`,
		"turns too many":     `{"protocol":1,"cli":"claude","max_turns":100000,"timeout_seconds":60}`,
		"no timeout":         `{"protocol":1,"cli":"claude"}`,
		"timeout too long":   `{"protocol":1,"cli":"claude","timeout_seconds":86401}`,
		"trailing data":      `{"protocol":1,"cli":"claude","timeout_seconds":60}{}`,
		"not json":           `cli=claude`,
		"wrong type":         `{"protocol":"1","cli":"claude","timeout_seconds":60}`,
		"oversized":          `{"protocol":1,"cli":"claude","timeout_seconds":60,"model":"` + strings.Repeat("a", MaxRequestBytes) + `"}`,
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
