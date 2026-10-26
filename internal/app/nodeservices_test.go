package app

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A platform's service addresses become the defaults for their settings;
// bad ones are skipped with a note.
func TestNodeDefaults(t *testing.T) {
	env := map[string]string{
		"BTCTAX_ELECTRUM_URL": "tcp://10.21.21.10:50001",
		"BTCTAX_OLLAMA_URL":   "http://ollama_ollama_1:11434/",
		"BTCTAX_CLN_URL":      "ftp://nope",
	}
	set, notes := NodeDefaults(func(k string) string { return env[k] })
	if set["fulcrumHost"] != "10.21.21.10" || set["fulcrumPort"] != "50001" || set["fulcrumTLS"] != "false" {
		t.Errorf("electrum: %v", set)
	}
	if set["aiUrl"] != "http://ollama_ollama_1:11434" {
		t.Errorf("ollama: %q", set["aiUrl"])
	}
	if _, ok := set["clnUrl"]; ok || len(notes) != 1 || !strings.Contains(notes[0], "BTCTAX_CLN_URL") {
		t.Errorf("bad CLN url: %v %v", set, notes)
	}
	for in, want := range map[string]string{"ssl://fulcrum.embassy:50002": "fulcrum.embassy:50002:true", "ssl://host": "host:50002:true", "tcp://host": "host:50001:false"} {
		h, p, tls, err := electrumURL(in)
		if got := h + ":" + p + ":" + map[bool]string{true: "true", false: "false"}[tls]; err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if _, _, _, err := electrumURL("http://host:50001"); err == nil {
		t.Error("http accepted as an electrum scheme")
	}
}

// The node's addresses fill in unset settings, a saved value wins, and
// Settings is told which ones come from the node.
func TestNodeServicesAsDefaults(t *testing.T) {
	st, _ := newTest(t)
	srv := New(st, nil)
	srv.UseNodeServices(map[string]string{"fulcrumHost": "electrs.local", "fulcrumTLS": "false", "aiUrl": "http://ollama:11434"})
	set := srv.settings()
	if set["fulcrumHost"] != "electrs.local" || set["fulcrumTLS"] != "false" || set["aiUrl"] != "http://ollama:11434" || set["fulcrumPort"] != "50002" {
		t.Errorf("defaults: %v", set)
	}
	if got := srv.fromNode(); got != "aiUrl,fulcrumHost,fulcrumTLS" {
		t.Errorf("fromNode before saving: %q", got)
	}
	if err := st.SetSetting("aiUrl", "http://my-gpu-box:11434"); err != nil {
		t.Fatal(err)
	}
	if srv.setting("aiUrl") != "http://my-gpu-box:11434" || srv.fromNode() != "fulcrumHost,fulcrumTLS" {
		t.Errorf("saved value should win: %q, %q", srv.setting("aiUrl"), srv.fromNode())
	}
}

// The health check answers without a session and says nothing private.
func TestHealth(t *testing.T) {
	st, _ := newTest(t)
	h := New(st, nil).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"version"`) {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}
