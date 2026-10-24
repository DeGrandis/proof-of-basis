package app

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"btctax/internal/demo"
)

// fakeOllama answers like Ollama: it asks for one tool, then answers
// with text, streaming. It records what it was sent.
type fakeOllama struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (f *fakeOllama) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			w.Write([]byte(`{"version":"0.9.0"}`))
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"small:1b","size":1000},{"name":"tooly:8b","size":5000000000}]}`))
		case "/api/show":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["model"] == "tooly:8b" {
				w.Write([]byte(`{"capabilities":["completion","tools","thinking"],"model_info":{"llama.context_length":32768}}`))
			} else {
				w.Write([]byte(`{"capabilities":["completion"],"model_info":{}}`))
			}
		case "/api/chat":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.mu.Lock()
			f.reqs = append(f.reqs, in)
			n := len(f.reqs)
			f.mu.Unlock()
			msgs := in["messages"].([]any)
			last := msgs[len(msgs)-1].(map[string]any)
			if last["role"] == "user" && strings.Contains(last["content"].(string), "2024") {
				w.Write([]byte(`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_tax_year","arguments":{"year":2024}}}]},"done":false}` + "\n"))
				w.Write([]byte(`{"message":{"role":"assistant","content":""},"done":true}` + "\n"))
				return
			}
			_ = n
			for _, part := range []string{"Here ", "is ", "the answer."} {
				w.Write([]byte(`{"message":{"role":"assistant","content":"` + part + `"},"done":false}` + "\n"))
			}
			w.Write([]byte(`{"message":{"role":"assistant","content":""},"done":true}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	})
}

func readStream(t *testing.T, body string) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad stream line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestAssistantChatWithTools(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	fo := &fakeOllama{}
	ol := httptest.NewServer(fo.handler(t))
	defer ol.Close()

	// Off until set up.
	if w := call(h, "GET", "/api/ai/status", ""); !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("status before setup: %s", w.Body.String())
	}
	// Settings lists models, tool-capable first.
	w := call(h, "POST", "/api/ai/models", `{"url":"`+ol.URL+`"}`)
	var ml struct {
		Models []struct {
			Name  string
			Tools bool
		}
	}
	json.Unmarshal(w.Body.Bytes(), &ml)
	if w.Code != 200 || len(ml.Models) != 2 || ml.Models[0].Name != "tooly:8b" || !ml.Models[0].Tools || ml.Models[1].Tools {
		t.Fatalf("models: %d %s", w.Code, w.Body.String())
	}
	if w := call(h, "POST", "/api/settings", `{"aiUrl":"ftp://x"}`); w.Code != 400 {
		t.Fatalf("bad url accepted: %d", w.Code)
	}
	if w := call(h, "POST", "/api/settings", `{"aiUrl":"`+ol.URL+`","aiModel":"tooly:8b","aiCtx":"16384"}`); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if w := call(h, "GET", "/api/ai/status", ""); !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("status after setup: %s", w.Body.String())
	}

	// A question that needs a tool.
	w = call(h, "POST", "/api/ai/chats", "")
	var nc struct{ ID int64 }
	json.Unmarshal(w.Body.Bytes(), &nc)
	id := strings.TrimSpace(strings.Split(strings.TrimPrefix(w.Body.String(), `{"id":`), "}")[0])
	w = call(h, "POST", "/api/ai/chats/"+id+"/send", `{"content":"Why was my 2024 tax that much?","page":{"hash":"review","year":2024}}`)
	if w.Code != 200 {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	evs := readStream(t, w.Body.String())
	var text strings.Builder
	sawTool := false
	for _, e := range evs {
		switch e["type"] {
		case "delta":
			text.WriteString(e["text"].(string))
		case "tool":
			lk := e["lookup"].(map[string]any)
			sawTool = lk["name"] == "get_tax_year" && strings.Contains(lk["summary"].(string), "2024")
		case "error":
			t.Fatalf("stream error: %v", e)
		}
	}
	if !sawTool || !strings.Contains(text.String(), "Here is the answer.") || evs[len(evs)-1]["type"] != "done" {
		t.Fatalf("stream: tool=%v text=%q events=%v", sawTool, text.String(), evs)
	}
	// What Ollama got: the summary and page in the system prompt, the
	// tools, thinking off, the context size, and the tool's result.
	fo.mu.Lock()
	first, second := fo.reqs[0], fo.reqs[1]
	fo.mu.Unlock()
	sys := first["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, want := range []string{"## Accounts", "## Tax years", "The Review page", "Cold storage"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if len(first["tools"].([]any)) < 5 || first["think"] != false || first["options"].(map[string]any)["num_ctx"].(float64) != 16384 {
		t.Errorf("request shape: tools=%d think=%v options=%v", len(first["tools"].([]any)), first["think"], first["options"])
	}
	msgs := second["messages"].([]any)
	toolMsg := msgs[len(msgs)-1].(map[string]any)
	if toolMsg["role"] != "tool" || !strings.Contains(toolMsg["content"].(string), "2024:") {
		t.Errorf("tool result not passed back: %v", toolMsg)
	}
	for _, s := range []string{"xpub", "zpub", "clnRune"} {
		if strings.Contains(sys, s) {
			t.Errorf("system prompt mentions %s", s)
		}
	}

	// The chat was saved with a title from the question, and lookups.
	w = call(h, "GET", "/api/ai/chats/"+id, "")
	var got struct {
		Chat     struct{ Title string }
		Messages []struct{ Role, Content, Data string }
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Chat.Title != "Why was my 2024 tax that much?" || len(got.Messages) != 2 || got.Messages[1].Role != "assistant" ||
		!strings.Contains(got.Messages[1].Data, "get_tax_year") {
		t.Fatalf("saved chat: %s", w.Body.String())
	}
	// A follow-up carries the earlier turns.
	call(h, "POST", "/api/ai/chats/"+id+"/send", `{"content":"and before that?","page":{"hash":"overview"}}`)
	fo.mu.Lock()
	third := fo.reqs[len(fo.reqs)-1]
	fo.mu.Unlock()
	if n := len(third["messages"].([]any)); n != 4 { // system, user, assistant, user
		t.Errorf("follow-up sent %d messages", n)
	}
	// Rename, list, delete; a wipe removes chats too.
	if w := call(h, "POST", "/api/ai/chats/"+id+"/rename", `{"title":"Taxes"}`); w.Code != 200 {
		t.Fatalf("rename: %d", w.Code)
	}
	if w := call(h, "GET", "/api/ai/chats", ""); !strings.Contains(w.Body.String(), `"title":"Taxes"`) {
		t.Fatalf("list: %s", w.Body.String())
	}
	call(h, "POST", "/api/wipe", `{"confirm":"WIPE"}`)
	if w := call(h, "GET", "/api/ai/chats", ""); w.Body.String() != "[]\n" {
		t.Fatalf("wipe left chats: %s", w.Body.String())
	}
}

func TestAssistantTools(t *testing.T) {
	st, _ := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, err := srv.buildReport()
	if err != nil {
		t.Fatal(err)
	}
	tb := &toolbox{s: srv, rep: rep}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"search_transactions", map[string]any{"kind": "buy", "from": "2021-01-01", "to": "2021-12-31"}, "records match"},
		{"get_tax_year", map[string]any{"year": float64(2021)}, "sales (grouped"},
		{"get_account", map[string]any{"name": "cold storage"}, "Purchase lots"},
		{"list_review_items", map[string]any{}, "need a label"},
		{"what_if_sale", map[string]any{"account": "Cold storage", "btc": 0.01}, "Estimated extra tax"},
		{"get_transaction", map[string]any{"id": "nope"}, "no record"},
	}
	for _, c := range cases {
		lk, out := tb.run(c.name, c.args)
		if lk.Error != "" || !strings.Contains(out, c.want) {
			t.Errorf("%s: error=%q out=%.300s", c.name, lk.Error, out)
		}
	}
	if lk, _ := tb.run("get_account", map[string]any{"name": "Nope"}); lk.Error == "" {
		t.Error("unknown account not reported")
	}
	if lk, _ := tb.run("drop_tables", nil); lk.Error == "" {
		t.Error("unknown tool not refused")
	}
	// A sale's record shows the lots it used.
	for _, g := range rep.Graph {
		if g.Kind == "sell" {
			if out := eventDetail(rep, g.Key); !strings.Contains(out, "Lots used by this sale") {
				t.Errorf("sale detail lacks lots: %s", out)
			}
			break
		}
	}
}
