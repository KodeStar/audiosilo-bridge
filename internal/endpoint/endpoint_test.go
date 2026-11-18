package endpoint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "sk-test-secret-123"

// fake is an OpenAI-compatible endpoint: models answers GET /v1/models,
// responses POST /v1/responses (0: no route, 404).
type fake struct {
	models    func(w http.ResponseWriter, r *http.Request)
	responses int
	auth      []string
}

func (f *fake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models" && f.models != nil:
			f.models(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses" && f.responses != 0:
			w.WriteHeader(f.responses)
			_, _ = w.Write([]byte(`{"error":{"message":"model is required"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckListsModelsAndWhatTheySay(t *testing.T) {
	f := &fake{responses: http.StatusBadRequest, models: func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"qwen3:32b","object":"model"},
			{"id":"router/big","context_length":131072,"supported_parameters":["tools","temperature"]},
			{"id":"router/small","context_length":8192,"supported_parameters":["temperature"]},
			{"id":"vllm-model","max_model_len":32768},
			{"id":"flagged","capabilities":{"function_calling":true}},
			{"id":"listed","capabilities":["completion"]},
			{"id":"bad\nid"},
			{"id":""},
			{"id":"qwen3:32b"}
		]}`))
	}}
	srv := f.server(t)
	rep, err := Check(context.Background(), NewClient(), srv.URL+"/v1", testKey)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Model{}
	var ids []string
	for _, m := range rep.Models {
		got[m.ID] = m
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "flagged,listed,qwen3:32b,router/big,router/small,vllm-model" {
		t.Fatalf("models %v (sorted, a bad id or a repeat left out)", ids)
	}
	if m := got["qwen3:32b"]; m.ContextTokens != 0 || m.Tools != nil {
		t.Errorf("a model the endpoint says nothing of: %+v", m)
	}
	if m := got["router/big"]; m.ContextTokens != 131072 || m.Tools == nil || !*m.Tools {
		t.Errorf("router/big: %+v", m)
	}
	if m := got["router/small"]; m.ContextTokens != 8192 || m.Tools == nil || *m.Tools {
		t.Errorf("router/small: %+v", m)
	}
	if m := got["vllm-model"]; m.ContextTokens != 32768 {
		t.Errorf("vllm-model: %+v", m)
	}
	if m := got["flagged"]; m.Tools == nil || !*m.Tools {
		t.Errorf("flagged: %+v", m)
	}
	if m := got["listed"]; m.Tools == nil || *m.Tools {
		t.Errorf("listed: %+v", m)
	}
	if rep.Responses != ResponsesYes {
		t.Errorf("responses %q (a 400 for the missing model is the route answering)", rep.Responses)
	}
	for _, a := range f.auth {
		if a != "Bearer "+testKey {
			t.Errorf("Authorization %q", a)
		}
	}
}

func TestCheckWithoutKeySendsNoAuthorization(t *testing.T) {
	f := &fake{models: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) }}
	srv := f.server(t)
	rep, err := Check(context.Background(), NewClient(), srv.URL+"/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Models) != 0 || rep.Models == nil {
		t.Errorf("models %#v: an empty list, not nil", rep.Models)
	}
	if rep.Responses != ResponsesNo || !strings.Contains(rep.ResponsesDetail, "/responses") {
		t.Errorf("responses %q %q: a 404 is no route", rep.Responses, rep.ResponsesDetail)
	}
	for _, a := range f.auth {
		if a != "" {
			t.Errorf("Authorization %q with no key", a)
		}
	}
}

func TestCheckErrorsNeverHoldTheKey(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"refused": {http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided: ` + testKey + `"}}`, "refused the API key"},
		"server":  {http.StatusInternalServerError, `upstream said ` + testKey + "\n\x00 broke", "answered 500"},
		"string":  {http.StatusBadGateway, `{"error":"bad gateway for ` + testKey + `"}`, "answered 502"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{models: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}}
			srv := f.server(t)
			_, err := Check(context.Background(), NewClient(), srv.URL+"/v1", testKey)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), testKey) || strings.ContainsAny(err.Error(), "\n\x00") {
				t.Errorf("err %q holds the key or a control character", err)
			}
			if !strings.Contains(err.Error(), "[the key]") {
				t.Errorf("err %q: the endpoint's words, the key cut out", err)
			}
		})
	}
}

func TestCheckWrongAddress(t *testing.T) {
	f := &fake{}
	srv := f.server(t)
	_, err := Check(context.Background(), NewClient(), srv.URL+"/v2", "")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "ends before /models") {
		t.Fatalf("err %v", err)
	}
	notList := &fake{models: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>a web page</html>`)) }}
	srv = notList.server(t)
	if _, err := Check(context.Background(), NewClient(), srv.URL+"/v1", ""); err == nil ||
		!strings.Contains(err.Error(), "isn't an OpenAI-style model list") {
		t.Fatalf("a web page: %v", err)
	}
}

// A redirect is never followed (the key would go along), and says where to.
func TestCheckFollowsNoRedirect(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	t.Cleanup(target.Close)
	f := &fake{models: func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/models", http.StatusFound)
	}}
	srv := f.server(t)
	_, err := Check(context.Background(), NewClient(), srv.URL+"/v1", testKey)
	if err == nil || !strings.Contains(err.Error(), "redirects to "+target.URL) {
		t.Fatalf("err %v", err)
	}
	if followed {
		t.Error("the redirect was followed")
	}
}

func TestCheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL + "/v1"
	srv.Close()
	if _, err := Check(context.Background(), NewClient(), base, testKey); err == nil ||
		!strings.HasPrefix(err.Error(), "couldn't reach the endpoint") || strings.Contains(err.Error(), testKey) {
		t.Fatalf("err %v", err)
	}
}

func TestResponsesUnknownOnARefusal(t *testing.T) {
	f := &fake{responses: http.StatusUnauthorized, models: func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}}
	srv := f.server(t)
	rep, err := Check(context.Background(), NewClient(), srv.URL+"/v1", testKey)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Responses != ResponsesUnknown || !strings.Contains(rep.ResponsesDetail, "401") {
		t.Errorf("responses %q %q", rep.Responses, rep.ResponsesDetail)
	}
}

func TestCleanBounds(t *testing.T) {
	long := strings.Repeat("é", maxMessage)
	got := clean(long, "")
	if len(got) > maxMessage+3 || !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "é") {
		t.Errorf("clean: %d bytes", len(got))
	}
	if got := clean("a\r\n\tb   c", ""); got != "a b c" {
		t.Errorf("clean folds space: %q", got)
	}
}
