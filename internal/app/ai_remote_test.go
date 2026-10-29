package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DeGrandis/proof-of-basis/internal/demo"
)

// fakeProvider answers like a hosted provider in the OpenAI format: a tool
// call for a question about 2024, then text once the result is back. It
// records what it was sent and with which key.
type fakeProvider struct {
	mu   sync.Mutex
	reqs []map[string]any
	keys []string
}

func (f *fakeProvider) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("Authorization"))
		f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"big/model"},{"id":"another/model"}]}`)
		case "/v1/chat/completions":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.mu.Lock()
			f.reqs = append(f.reqs, in)
			f.mu.Unlock()
			msgs := in["messages"].([]any)
			last := msgs[len(msgs)-1].(map[string]any)
			w.Header().Set("Content-Type", "text/event-stream")
			if last["role"] == "user" && strings.Contains(last["content"].(string), "2024") {
				io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"get_tax_year","arguments":"{\"year\":2024}"}}]}}]}`+"\n\ndata: [DONE]\n\n")
				return
			}
			for _, part := range []string{"Here ", "is ", "the answer."} {
				io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+part+`"}}]}`+"\n\n")
			}
			io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	})
}

func TestAssistantOnAHostedProvider(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{}
	srv := httptest.NewServer(fp.handler())
	defer srv.Close()
	base := srv.URL + "/v1"
	var set map[string]string
	settings := func() {
		w := call(h, "GET", "/api/settings", "")
		set = nil
		json.Unmarshal(w.Body.Bytes(), &set)
		if strings.Contains(w.Body.String(), "sk-secret") {
			t.Fatalf("the key came back: %s", w.Body.String())
		}
	}

	// Local is the default, and the sentence to type comes from the server.
	settings()
	if set["aiMode"] != "local" || set["aiRemotePhrase"] != remoteConsent || set["aiRemoteKeySet"] != "false" {
		t.Fatalf("defaults: %v", set)
	}
	remote := `"aiMode":"remote","aiRemoteUrl":"` + base + `","aiRemoteModel":"big/model","aiRemoteKey":"sk-secret"`
	// Without the sentence, or with another one, nothing changes.
	for _, body := range []string{`{` + remote + `}`, `{` + remote + `,"aiRemoteAccept":"I accept"}`} {
		if w := call(h, "POST", "/api/settings", body); w.Code != 400 || !strings.Contains(w.Body.String(), remoteConsent) {
			t.Fatalf("switched without the sentence: %d %s", w.Code, w.Body.String())
		}
	}
	settings()
	if set["aiMode"] != "local" || set["aiRemoteKeySet"] != "false" || set["aiRemoteConsent"] != "" {
		t.Fatalf("a refused switch changed settings: %v", set)
	}
	// Plain http to a public host is refused, so a key isn't sent in the clear.
	if w := call(h, "POST", "/api/settings", `{"aiRemoteUrl":"http://example.com/v1"}`); w.Code != 400 {
		t.Errorf("plain http to a public host accepted: %d", w.Code)
	}
	// With it (any case, any spacing), the switch is saved and the key kept hidden.
	if w := call(h, "POST", "/api/settings", `{`+remote+`,"aiRemoteAccept":"  i accept my DATA leaking   to a provider "}`); w.Code != 200 {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}
	settings()
	if set["aiMode"] != "remote" || set["aiRemoteKeySet"] != "true" || set["aiRemoteConsent"] == "" || set["aiRemoteModel"] != "big/model" {
		t.Fatalf("after the switch: %v", set)
	}
	if w := call(h, "GET", "/api/actions", ""); strings.Contains(w.Body.String(), "sk-secret") || !strings.Contains(w.Body.String(), "hosted provider") {
		t.Errorf("activity: %s", w.Body.String())
	}
	if w := call(h, "GET", "/api/ai/status", ""); !strings.Contains(w.Body.String(), `"remote":true`) || !strings.Contains(w.Body.String(), `"host":"127.0.0.1"`) {
		t.Errorf("status: %s", w.Body.String())
	}
	// Settings lists the provider's models with the saved key.
	w := call(h, "POST", "/api/ai/models", `{"url":"`+base+`","remote":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "another/model") {
		t.Fatalf("models: %d %s", w.Code, w.Body.String())
	}
	// A provider without a model list is tested by asking the typed model.
	nolist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fp.handler().ServeHTTP(w, r)
	}))
	defer nolist.Close()
	if w := call(h, "POST", "/api/ai/models", `{"url":"`+nolist.URL+`/v1","key":"other","remote":true}`); w.Code != 400 || !strings.Contains(w.Body.String(), "Type the model") {
		t.Errorf("no list, no model: %d %s", w.Code, w.Body.String())
	}
	if w := call(h, "POST", "/api/ai/models", `{"url":"`+nolist.URL+`/v1","key":"other","model":"big/model","remote":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"answered":"big/model"`) {
		t.Errorf("no list, with a model: %d %s", w.Code, w.Body.String())
	}
	fp.mu.Lock()
	fp.reqs, fp.keys = nil, nil // the test's "Reply with OK." isn't part of the chat below
	fp.mu.Unlock()

	// A question that needs a tool goes to the provider, and the result
	// answers the call by its id.
	w = call(h, "POST", "/api/ai/chats", "")
	id := strings.TrimSpace(strings.Split(strings.TrimPrefix(w.Body.String(), `{"id":`), "}")[0])
	w = call(h, "POST", "/api/ai/chats/"+id+"/send", `{"content":"Why was my 2024 tax that much?","page":{"hash":"review","year":2024}}`)
	var text strings.Builder
	sawTool := false
	for _, e := range readStream(t, w.Body.String()) {
		switch e["type"] {
		case "delta":
			text.WriteString(e["text"].(string))
		case "tool":
			sawTool = e["lookup"].(map[string]any)["name"] == "get_tax_year"
		case "error":
			t.Fatalf("stream error: %v", e)
		}
	}
	if !sawTool || text.String() != "Here is the answer." {
		t.Fatalf("answer: tool=%v text=%q", sawTool, text.String())
	}
	fp.mu.Lock()
	reqs, keys := fp.reqs, fp.keys
	fp.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("%d requests to the provider", len(reqs))
	}
	for _, k := range keys {
		if k != "Bearer sk-secret" {
			t.Errorf("key sent as %q", k)
		}
	}
	first, second := reqs[0], reqs[1]
	if first["model"] != "big/model" || len(first["tools"].([]any)) < 5 || first["options"] != nil || first["think"] != nil {
		t.Errorf("request shape: %v", first)
	}
	msgs := second["messages"].([]any)
	call0 := msgs[len(msgs)-2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	result := msgs[len(msgs)-1].(map[string]any)
	if call0["id"] != "call_x" || result["role"] != "tool" || result["tool_call_id"] != "call_x" || !strings.Contains(result["content"].(string), "2024:") {
		t.Errorf("tool round: call %v, result %v", call0, result)
	}
	w = call(h, "GET", "/api/ai/chats/"+id, "")
	if !strings.Contains(w.Body.String(), `"model":"big/model"`) {
		t.Errorf("chat saved without the provider's model: %s", w.Body.String())
	}

	// Back to local is one click; going remote again asks again.
	if w := call(h, "POST", "/api/settings", `{"aiMode":"local"}`); w.Code != 200 {
		t.Fatalf("to local: %d", w.Code)
	}
	if w := call(h, "POST", "/api/settings", `{"aiMode":"remote"}`); w.Code != 400 {
		t.Errorf("remote again without the sentence: %d", w.Code)
	}
	// Turning it off forgets the key.
	call(h, "POST", "/api/settings", `{"aiMode":"local","aiUrl":"","aiModel":"","aiRemoteForget":"1"}`)
	settings()
	if set["aiRemoteKeySet"] != "false" || set["aiRemoteUrl"] != "" || set["aiMode"] != "local" {
		t.Errorf("after turning off: %v", set)
	}
}
