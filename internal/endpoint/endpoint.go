// Package endpoint checks an OpenAI-compatible endpoint for the page's Test
// connection (bridge#84): the models it offers (GET <base>/models), what each
// says about its context and tool calling where the endpoint says it, and
// whether it offers the Responses API the agent's codex needs (a POST of an
// empty object to <base>/responses, which a server with the route refuses for
// the missing model without running anything).
//
// The Bridge runs the check, not the agent container: the container has no way
// to answer a request, only staged jobs. Both reach the network the same way
// (each through its own Docker bridge network and the host), so an address the
// Bridge reaches is one the agent reaches, unless a firewall says otherwise.
//
// The API key goes in the Authorization header only, and is cut from anything
// the endpoint answers before that reaches the page.
package endpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// Limits on the check.
const (
	// Timeout bounds each of the two requests.
	Timeout = 15 * time.Second
	// maxBody bounds what is read of an answer.
	maxBody = 4 << 20
	// maxModels bounds the models listed, maxModelID a model's id.
	maxModels  = 1000
	maxModelID = 200
	// maxMessage bounds an error's words from the endpoint.
	maxMessage = 300
)

// The Responses API answers (Report.Responses).
const (
	ResponsesYes     = "yes"
	ResponsesNo      = "no"
	ResponsesUnknown = "unknown"
)

// Model is one model the endpoint lists. ContextTokens is 0 and Tools nil
// when the endpoint doesn't say (OpenAI, Ollama and LM Studio don't; OpenRouter,
// Groq and vLLM say some of it).
type Model struct {
	ID            string `json:"id"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	Tools         *bool  `json:"tools,omitempty"`
}

// Report is what the check found (the page's answer, api.EndpointCheck).
type Report struct {
	Models []Model `json:"models"`
	// Responses is whether the endpoint offers the Responses API (Responses*),
	// and ResponsesDetail why it is no or unknown.
	Responses       string `json:"responses"`
	ResponsesDetail string `json:"responses_detail"`
}

// NewClient is the HTTP client the check uses: bounded, and following no
// redirect (the Authorization header must go to the address given, and a
// redirect means the address is not the one to set).
func NewClient() *http.Client {
	return &http.Client{
		Timeout:       Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Check lists base's models and asks whether it offers the Responses API.
// base is a checked base URL (agentproto.CheckBaseURL) without a trailing
// slash; key is the endpoint's API key, "" for none. An error is a sentence
// for the page (the models couldn't be listed), never holding the key.
func Check(ctx context.Context, client *http.Client, base, key string) (Report, error) {
	models, err := listModels(ctx, client, base, key)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Models: models}
	rep.Responses, rep.ResponsesDetail = probeResponses(ctx, client, base, key)
	return rep, nil
}

// listModels is GET <base>/models.
func listModels(ctx context.Context, client *http.Client, base, key string) ([]Model, error) {
	status, body, location, err := do(ctx, client, http.MethodGet, base+"/models", nil, key)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, fmt.Errorf("the endpoint refused the API key (%d %s): check the key%s",
			status, http.StatusText(status), said(body, key))
	case status == http.StatusNotFound:
		return nil, fmt.Errorf("the endpoint has no %s/models (404): check the address, which ends before /models "+
			"(like https://api.openai.com/v1 or http://192.168.1.20:11434/v1)", base)
	case status >= 300 && status < 400:
		where := ""
		if location != "" {
			where = " to " + clean(location, key)
		}
		return nil, fmt.Errorf("the endpoint redirects%s (%d): set the address it redirects to", where, status)
	case status != http.StatusOK:
		return nil, fmt.Errorf("the endpoint answered %d %s%s", status, http.StatusText(status), said(body, key))
	}
	models, err := parseModels(body)
	if err != nil {
		return nil, errors.New("the endpoint's /models answer isn't an OpenAI-style model list: check the address")
	}
	return models, nil
}

// probeResponses is POST <base>/responses with an empty object: 404 or 405 say
// the route isn't there; 400 or 422 (the model is missing) and a success say it
// is; anything else (a refused key, a server error) can't tell.
func probeResponses(ctx context.Context, client *http.Client, base, key string) (answer, detail string) {
	status, body, _, err := do(ctx, client, http.MethodPost, base+"/responses", []byte("{}"), key)
	switch {
	case err != nil:
		return ResponsesUnknown, err.Error()
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return ResponsesNo, fmt.Sprintf("the endpoint has no %s/responses (%d): the agent needs the Responses API, "+
			"which older servers don't offer: update the server, or use one that offers it", base, status)
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || status/100 == 2:
		return ResponsesYes, ""
	}
	return ResponsesUnknown, fmt.Sprintf("the endpoint answered %d %s to a check of %s/responses%s",
		status, http.StatusText(status), base, said(body, key))
}

// do sends one request and reads its answer (bounded). An error is the
// endpoint not answering at all.
func do(ctx context.Context, client *http.Client, method, url string, body []byte, key string) (status int, answer []byte, location string, err error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, "", errors.New("the endpoint's address isn't usable")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, "", fmt.Errorf("couldn't reach the endpoint: %s", clean(unwrapURL(err).Error(), key))
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, "", fmt.Errorf("the endpoint's answer broke off: %s", clean(err.Error(), key))
	}
	return resp.StatusCode, answer, resp.Header.Get("Location"), nil
}

// unwrapURL drops the *url.Error wrapper, whose text repeats the method and
// the address the sentence names already.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// modelEntry is one entry of a /models list, with the fields some endpoints
// add about a model: its context (context_length: OpenRouter, Together;
// context_window: Groq; max_context_length: LM Studio; max_model_len: vLLM)
// and its tool calling (supported_parameters: OpenRouter; capabilities, a
// list or an object of flags).
type modelEntry struct {
	ID                  string          `json:"id"`
	ContextLength       int             `json:"context_length"`
	ContextWindow       int             `json:"context_window"`
	MaxContextLength    int             `json:"max_context_length"`
	MaxModelLen         int             `json:"max_model_len"`
	SupportedParameters []string        `json:"supported_parameters"`
	Capabilities        json.RawMessage `json:"capabilities"`
}

// parseModels reads a /models answer: {"data": [...]} (OpenAI's shape, which
// the compatible servers copy) or {"models": [...]}. Models come out sorted by
// id, at most maxModels, each id printable and bounded (others are left out).
func parseModels(body []byte) ([]Model, error) {
	var list struct {
		Data   *[]modelEntry `json:"data"`
		Models *[]modelEntry `json:"models"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	entries := list.Data
	if entries == nil {
		entries = list.Models
	}
	if entries == nil {
		return nil, errors.New("no data")
	}
	out := []Model{}
	seen := map[string]bool{}
	for _, e := range *entries {
		if e.ID == "" || len(e.ID) > maxModelID || !printable(e.ID) || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		m := Model{ID: e.ID, ContextTokens: max(e.ContextLength, e.ContextWindow, e.MaxContextLength, e.MaxModelLen, 0)}
		if e.SupportedParameters != nil {
			m.Tools = new(slices.Contains(e.SupportedParameters, "tools"))
		}
		if t, ok := capabilityTools(e.Capabilities); ok {
			m.Tools = new(t)
		}
		out = append(out, m)
		if len(out) == maxModels {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// capabilityTools reads a capabilities field: a list (["completion","tools"])
// or an object of flags ({"tools": true} or {"function_calling": true}). ok is
// false when it says nothing about tools.
func capabilityTools(raw json.RawMessage) (tools, ok bool) {
	if len(raw) == 0 {
		return false, false
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return slices.Contains(list, "tools") || slices.Contains(list, "function_calling"), true
	}
	var flags map[string]any
	if json.Unmarshal(raw, &flags) == nil {
		for _, k := range []string{"tools", "function_calling", "tool_calling"} {
			if b, isBool := flags[k].(bool); isBool {
				return b, true
			}
		}
	}
	return false, false
}

// said is the endpoint's own words about an error, when it gave some: the
// OpenAI-style {"error": {"message": ...}} or {"error": "..."}, else the
// answer's start, cleaned and bounded, after ": ".
func said(body []byte, key string) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	text := ""
	if json.Unmarshal(body, &e) == nil && len(e.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		var s string
		switch {
		case json.Unmarshal(e.Error, &obj) == nil && obj.Message != "":
			text = obj.Message
		case json.Unmarshal(e.Error, &s) == nil:
			text = s
		}
	}
	if text == "" && !bytes.HasPrefix(bytes.TrimSpace(body), []byte("<")) {
		text = string(body)
	}
	text = clean(text, key)
	if text == "" {
		return ""
	}
	return ": " + text
}

// clean makes text fit for the page: the key cut out, control characters and
// runs of space folded to one space, at most maxMessage bytes.
func clean(text, key string) string {
	if key != "" {
		text = strings.ReplaceAll(text, key, "[the key]")
	}
	text = strings.Join(strings.FieldsFunc(text, func(r rune) bool { return r < ' ' || r == 0x7f || r == ' ' }), " ")
	if len(text) > maxMessage {
		text = strings.ToValidUTF8(text[:maxMessage], "") + "..." // never a cut UTF-8 sequence
	}
	return text
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}
