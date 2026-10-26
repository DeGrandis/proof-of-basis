package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"btctax/internal/ollama"
)

type fakeAI struct{ calls int }

func (f *fakeAI) Name() string { return "Fake model" }
func (f *fakeAI) Chat(ctx context.Context, req ollama.ChatRequest, onDelta, onThink func(string)) (ollama.Message, error) {
	f.calls++
	onDelta("Hello from the demo.")
	return ollama.Message{Role: "assistant", Content: "Hello from the demo."}, nil
}

func newHub(t *testing.T, cfg Config) (*Hub, *httptest.Server) {
	cfg.Dir = t.TempDir()
	cfg.Static = fstest.MapFS{"index.html": {Data: []byte("<html>demo</html>")}, "app.js": {Data: []byte("1")}}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

func visitor(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func get(t *testing.T, c *http.Client, url string, out any) int {
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func post(t *testing.T, c *http.Client, url, body string) (int, string) {
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

// Each visitor has their own copy of the demo data, with no login; the
// node is simulated and uploads are refused.
func TestVisitorsGetTheirOwnCopy(t *testing.T) {
	h, srv := newHub(t, Config{AIPerVisitor: 2, AIPerDay: 10})
	a, b := visitor(t), visitor(t)
	var rep struct {
		Settings  map[string]string
		Unmatched []struct{ Key string }
		Lightning *struct{ Events []any }
	}
	if code := get(t, a, srv.URL+"/api/report", &rep); code != 200 {
		t.Fatalf("report without a login: %d", code)
	}
	if rep.Settings["demo"] != "true" || len(rep.Unmatched) == 0 || rep.Lightning == nil || len(rep.Lightning.Events) == 0 {
		t.Fatalf("demo data: demo=%q unmatched=%d lightning=%v", rep.Settings["demo"], len(rep.Unmatched), rep.Lightning != nil)
	}
	before := len(rep.Unmatched)
	if code, body := post(t, a, srv.URL+"/api/labels", `{"Key":"`+rep.Unmatched[0].Key+`","Category":"gift"}`); code != 200 {
		t.Fatalf("label: %d %s", code, body)
	}
	get(t, a, srv.URL+"/api/report", &rep)
	afterA := len(rep.Unmatched)
	get(t, b, srv.URL+"/api/report", &rep)
	if afterA != before-1 || len(rep.Unmatched) != before {
		t.Errorf("visitor A labeled one: A has %d, B has %d, both started with %d", afterA, len(rep.Unmatched), before)
	}
	if h.Copies() != 2 {
		t.Errorf("copies: %d", h.Copies())
	}

	var st struct {
		OK     bool
		Height int64
	}
	get(t, a, srv.URL+"/api/fulcrum/status", &st)
	if !st.OK || st.Height < 840_000 {
		t.Errorf("simulated node: %+v", st)
	}
	// A wallet the visitor adds "scans" as empty.
	_, body := post(t, a, srv.URL+"/api/wallets", `{"name":"Mine","key":"zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"}`)
	var added struct{ ID int64 }
	json.Unmarshal([]byte(body), &added)
	if code, body := post(t, a, srv.URL+"/api/wallets/"+itoa(added.ID)+"/scan", "{}"); code != 200 || !strings.Contains(body, `"txs":0`) {
		t.Errorf("scan of a new wallet: %d %s", code, body)
	}
	for _, p := range []string{"/api/imports", "/api/attachments", "/api/restore", "/api/prices/fetch", "/api/wipe", "/api/auth/password"} {
		if code, _ := post(t, a, srv.URL+p, "{}"); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", p, code)
		}
	}
	// The node and assistant settings stay fixed.
	post(t, a, srv.URL+"/api/settings", `{"fulcrumHost":"169.254.169.254","aiUrl":"http://example.com","timezone":"America/New_York"}`)
	var set map[string]string
	get(t, a, srv.URL+"/api/settings", &set)
	if set["fulcrumHost"] != "" || set["aiUrl"] != "" || set["timezone"] != "America/New_York" {
		t.Errorf("settings: host %q ai %q tz %q", set["fulcrumHost"], set["aiUrl"], set["timezone"])
	}

	// Start over: a fresh copy.
	post(t, a, srv.URL+"/api/demo/reset", "")
	get(t, a, srv.URL+"/api/report", &rep)
	if len(rep.Unmatched) != before {
		t.Errorf("after starting over: %d unmatched, want %d", len(rep.Unmatched), before)
	}
}

// The assistant answers within each visitor's allowance and the day's.
func TestAssistantAllowance(t *testing.T) {
	ai := &fakeAI{}
	_, srv := newHub(t, Config{AI: ai, AIPerVisitor: 2, AIPerDay: 3})
	ask := func(c *http.Client) int {
		_, body := post(t, c, srv.URL+"/api/ai/chats", "{}")
		var chat struct{ ID int64 }
		json.Unmarshal([]byte(body), &chat)
		code, _ := post(t, c, srv.URL+"/api/ai/chats/"+itoa(chat.ID)+"/send", `{"content":"hi","page":{}}`)
		return code
	}
	a, b := visitor(t), visitor(t)
	var status struct {
		Enabled bool
		Model   string
	}
	get(t, a, srv.URL+"/api/ai/status", &status)
	if !status.Enabled || status.Model != "Fake model" {
		t.Fatalf("status %+v", status)
	}
	got := []int{ask(a), ask(a), ask(a), ask(b), ask(b)}
	want := []int{200, 200, 429, 200, 429}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("answers %v, want %v", got, want)
		}
	}
	if ai.calls != 3 {
		t.Errorf("model calls: %d", ai.calls)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
