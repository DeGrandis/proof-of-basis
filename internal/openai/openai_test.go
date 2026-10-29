package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
)

// A provider's stream as the OpenAI format sends it: reasoning, text, and a
// tool call whose arguments arrive in pieces, then [DONE].
const toolStream = `: keep-alive

data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"Look it up. "}}]}

data: {"choices":[{"delta":{"content":"Checking."}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_tax_year","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ye"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ar\": 2024}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`

func TestChatStreamsTextReasoningAndToolCalls(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, toolStream)
	}))
	defer srv.Close()
	c, err := New(srv.URL+"/v1/", "sk-test", "some/model")
	if err != nil {
		t.Fatal(err)
	}
	var text, think strings.Builder
	msg, err := c.Chat(context.Background(), ollama.ChatRequest{
		Messages: []ollama.Message{{Role: "system", Content: "You help."}, {Role: "user", Content: "2024 tax?"}},
		Tools:    []ollama.Tool{{Type: "function", Function: ollama.ToolSpec{Name: "get_tax_year", Parameters: map[string]any{"type": "object"}}}},
		Options:  map[string]any{"temperature": 0.2, "num_ctx": 16384},
	}, func(d string) { text.WriteString(d) }, func(d string) { think.WriteString(d) })
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-test" {
		t.Errorf("auth header %q", auth)
	}
	if got["model"] != "some/model" || got["stream"] != true || got["temperature"] != 0.2 || got["num_ctx"] != nil {
		t.Errorf("request: %v", got)
	}
	if text.String() != "Checking." || think.String() != "Look it up. " || msg.Content != "Checking." {
		t.Errorf("text %q, reasoning %q", text.String(), think.String())
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_tax_year" || msg.ToolCalls[0].Function.Arguments["year"] != 2024.0 {
		t.Fatalf("tool calls: %+v", msg.ToolCalls)
	}
	if ids, _ := msg.Raw.([]string); len(ids) != 1 || ids[0] != "call_abc" {
		t.Errorf("ids kept: %v", msg.Raw)
	}
}

// The next round sends the call back with the provider's id, and the result
// answers it by that id; calls from earlier answers get made-up ids.
func TestToMessagesPairsResultsWithCalls(t *testing.T) {
	var call ollama.ToolCall
	call.Function.Name = "accounts"
	call.Function.Arguments = map[string]any{"name": "Coldcard"}
	out := toMessages([]ollama.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ollama.ToolCall{call, call}, Raw: []string{"call_1"}},
		{Role: "tool", ToolName: "accounts", Content: "first"},
		{Role: "tool", ToolName: "accounts", Content: "second"},
		{Role: "system", Content: "a correction"},
		{Role: "tool", Content: "stray"},
	})
	b, _ := json.Marshal(out)
	s := string(b)
	for _, want := range []string{
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"accounts","arguments":"{\"name\":\"Coldcard\"}"}},{"id":"call_app_2_1"`,
		`{"role":"tool","content":"first","tool_call_id":"call_1"}`,
		`{"role":"tool","content":"second","tool_call_id":"call_app_2_1"}`,
		`{"role":"system","content":"a correction"}`,
		`{"role":"user","content":"[Lookup result] stray"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s\nin %s", want, s)
		}
	}
}

func TestModelsAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer good":
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"Invalid API key"}}`)
		case r.URL.Path == "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"z-model"},{"id":"a-model"}]}`)
		default:
			w.WriteHeader(429)
			io.WriteString(w, `{"error":"slow down"}`)
		}
	}))
	defer srv.Close()
	good, _ := New(srv.URL+"/v1", "good", "a-model")
	ms, known, err := good.Models(context.Background())
	if err != nil || known || len(ms) != 2 || ms[0].Name != "a-model" || !ms[0].Tools {
		t.Fatalf("models: %v %v %v", ms, known, err)
	}
	if _, err := good.Chat(context.Background(), ollama.ChatRequest{}, nil, nil); err == nil || !strings.Contains(err.Error(), "limiting requests (slow down)") {
		t.Errorf("429: %v", err)
	}
	bad, _ := New(srv.URL+"/v1", "bad", "a-model")
	if _, _, err := bad.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "didn't accept the API key (Invalid API key)") {
		t.Errorf("401: %v", err)
	}
}

// Where the list says which parameters each model takes (OpenRouter), only
// those with "tools" can be the assistant; a provider with no list at all
// is told apart from one that refused.
func TestModelsSayWhichCallTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/nolist") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[{"id":"chat-only","supported_parameters":["temperature"]},{"id":"agent","context_length":131072,"supported_parameters":["tools","temperature"]}]}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL+"/v1", "", "")
	ms, known, err := c.Models(context.Background())
	if err != nil || !known || len(ms) != 2 || ms[0].Name != "agent" || !ms[0].Tools || ms[0].Context != 131072 || ms[1].Tools {
		t.Errorf("models: %+v %v %v", ms, known, err)
	}
	none, _ := New(srv.URL+"/nolist", "", "")
	if _, _, err := none.Models(context.Background()); !errors.Is(err, ErrNoModelList) {
		t.Errorf("no list: %v", err)
	}
}

// A model that only takes its default temperature (OpenAI's reasoning
// models) gets the request again without one, and later ones leave it out.
func TestRetriesWithoutTemperature(t *testing.T) {
	var temps []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		temps = append(temps, in["temperature"])
		if in["temperature"] != nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"Unsupported value: 'temperature' does not support 0.2 with this model. Only the default (1) value is supported.","param":"temperature","code":"unsupported_value"}}`)
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Fine.\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "o-model")
	req := ollama.ChatRequest{Options: map[string]any{"temperature": 0.2}}
	for i := 0; i < 2; i++ {
		if msg, err := c.Chat(context.Background(), req, nil, nil); err != nil || msg.Content != "Fine." {
			t.Fatalf("round %d: %v %+v", i, err, msg)
		}
	}
	if len(temps) != 3 || temps[0] != 0.2 || temps[1] != nil || temps[2] != nil {
		t.Errorf("temperatures sent: %v", temps)
	}
	// Other refusals aren't retried.
	other, _ := New(srv.URL, "", "o-model-2")
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		temps = append(temps, "x")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"context too long"}}`)
	})
	if _, err := other.Chat(context.Background(), req, nil, nil); err == nil || len(temps) != 4 {
		t.Errorf("retried another refusal: %v %v", err, temps)
	}
}

func TestStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hal\"}}]}\n\ndata: {\"error\":{\"message\":\"upstream overloaded\"}}\n\n")
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "m")
	msg, err := c.Chat(context.Background(), ollama.ChatRequest{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "upstream overloaded") {
		t.Errorf("err %v (msg %+v)", err, msg)
	}
}

func TestNewChecksTheAddress(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "openrouter.ai", "http://example.com/v1"} {
		if _, err := New(bad, "k", "m"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"https://openrouter.ai/api/v1", "https://api.mistral.ai/v1/chat/completions", "http://localhost:1234/v1", "http://127.0.0.1:8080/v1", "http://192.168.1.20:1234/v1"} {
		c, err := New(ok, "k", "m")
		if err != nil {
			t.Errorf("%q refused: %v", ok, err)
			continue
		}
		if strings.HasSuffix(c.base, "/chat/completions") {
			t.Errorf("%q kept the endpoint: %s", ok, c.base)
		}
	}
}

// Some providers answer all at once even when asked to stream, or send an
// error with a 200 status; both have to reach the user.
func TestReplyWithoutAStream(t *testing.T) {
	for _, tc := range []struct{ body, text, err string }{
		{`{"choices":[{"message":{"role":"assistant","content":"Hi there.","tool_calls":[{"id":"c1","type":"function","function":{"name":"accounts","arguments":"{}"}}]}}]}`, "Hi there.", ""},
		{`{"error":{"message":"This model does not support tools","code":400}}`, "", "the provider: This model does not support tools"},
		{`{"error":{"message":"Provider returned error","code":429,"metadata":{"raw":"m:free is temporarily rate-limited upstream."}}}`, "", "the provider: m:free is temporarily rate-limited upstream."},
		{`{"error":{"message":"Provider returned error","metadata":{"raw":"{\"error\":{\"message\":\"Invalid tool schema\"}}"}}}`, "", "the provider: Invalid tool schema"},
		{`<html>Bad gateway</html>`, "", "can't read: <html>Bad gateway</html>"},
		{``, "", "empty reply"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, tc.body)
		}))
		c, _ := New(srv.URL, "", "m")
		var text strings.Builder
		msg, err := c.Chat(context.Background(), ollama.ChatRequest{}, func(d string) { text.WriteString(d) }, nil)
		srv.Close()
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err %v", tc.body, err)
			}
			continue
		}
		if err != nil || text.String() != tc.text || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "accounts" {
			t.Errorf("%s: %v %q %+v", tc.body, err, text.String(), msg)
		}
	}
}
