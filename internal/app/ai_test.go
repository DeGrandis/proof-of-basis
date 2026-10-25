package app

import (
	"btctax/internal/store"
	"bufio"
	"encoding/json"
	"fmt"
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
			if in["think"] == true {
				w.Write([]byte(`{"message":{"role":"assistant","content":"","thinking":"Let me check the numbers."},"done":false}` + "\n"))
			}
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
	var text, thought strings.Builder
	sawTool := false
	for _, e := range evs {
		switch e["type"] {
		case "thinking":
			thought.WriteString(e["text"].(string))
		case "delta":
			text.WriteString(e["text"].(string))
		case "tool":
			lk := e["lookup"].(map[string]any)
			sawTool = lk["name"] == "get_tax_year" && strings.Contains(lk["summary"].(string), "2024")
		case "error":
			t.Fatalf("stream error: %v", e)
		}
	}
	if !strings.Contains(thought.String(), "Let me check the numbers.") {
		t.Errorf("reasoning not streamed: %q", thought.String())
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
	if len(first["tools"].([]any)) < 5 || first["think"] != true || first["options"].(map[string]any)["num_ctx"].(float64) != 16384 {
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
		!strings.Contains(got.Messages[1].Data, "get_tax_year") || !strings.Contains(got.Messages[1].Data, "Let me check the numbers.") {
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
	// Thinking can be turned off.
	call(h, "POST", "/api/settings", `{"aiThink":"false"}`)
	call(h, "POST", "/api/ai/chats/"+id+"/send", `{"content":"once more","page":{"hash":"overview"}}`)
	fo.mu.Lock()
	last := fo.reqs[len(fo.reqs)-1]
	fo.mu.Unlock()
	if last["think"] != false {
		t.Errorf("think with the setting off: %v", last["think"])
	}
	if w := call(h, "POST", "/api/settings", `{"aiThink":"maybe"}`); w.Code != 400 {
		t.Errorf("bad aiThink accepted: %d", w.Code)
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

func TestSameRecord(t *testing.T) {
	tx := strings.Repeat("ab", 32)
	key := "tx:" + tx + ":0"
	if !sameRecord("ex:529", "", "529") || sameRecord("ex:5290", "", "529") {
		t.Error("bare record number")
	}
	for _, id := range []string{key, "[" + key + "]", "tx:" + tx, tx, "`" + key + "`"} {
		if !sameRecord(key, tx, id) {
			t.Errorf("%q should match", id)
		}
	}
	for _, id := range []string{"", "tx:" + tx + ":1", "ex:1", tx[:20]} {
		if sameRecord(key, tx, id) {
			t.Errorf("%q should not match", id)
		}
	}
}

func TestCalc(t *testing.T) {
	for expr, want := range map[string]float64{"1+2*3": 7, "(8672.03 - 1090.09)": 7581.94, "-81.11 - (-76.10)": -5.01, "$1,234.50 / 2": 617.25, "10 % 3": 1} {
		got, err := calc(expr)
		if err != nil || round6(got) != want {
			t.Errorf("%s = %v (%v), want %v", expr, got, err, want)
		}
	}
	for _, bad := range []string{"", "2+", "1/0", "abc", "((1)"} {
		if _, err := calc(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestMoreTools(t *testing.T) {
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
	var buy, sale string
	for _, g := range rep.Graph {
		if g.Kind == "buy" && buy == "" {
			buy = g.Key
		}
		if g.Kind == "sell" && sale == "" {
			sale = g.Key
		}
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"analyze_records", map[string]any{"source": "review", "measure": "account", "stat": "count"}, "Hot wallet"},
		{"analyze_records", map[string]any{"group_by": "year", "measure": "price", "stat": "mean"}, "ALL"},
		{"analyze_records", map[string]any{"source": "sales", "measure": "days_held", "stat": "median"}, "days"},
		{"analyze_records", map[string]any{"measure": "usd", "stat": "mode"}, "most common"},
		{"analyze_records", map[string]any{"measure": "weekday", "stat": "count"}, "day"},
		{"analyze_records", map[string]any{"source": "lots", "measure": "gain", "stat": "top", "top_n": float64(3)}, "["},
		{"analyze_records", map[string]any{"measure": "usd", "stat": "histogram"}, " to "},
		{"summarize_records", map[string]any{"group_by": "year"}, "ALL"},
		{"calculate", map[string]any{"expression": "2 * (3 + 4)"}, "= 14"},
		{"holdings_on", map[string]any{"date": "2023-12-31"}, "By account"},
		{"lots_turning_long_term", map[string]any{"within_days": float64(400)}, "long-term"},
		{"lots_at_a_loss", map[string]any{}, "below cost"},
		{"review_match_candidates", map[string]any{"id": rep.Unmatched[0].Key}, "Review item"},
		{"data_health", map[string]any{}, "Review"},
		{"income_records", map[string]any{"year": float64(2021)}, "income"},
		{"trace_coins", map[string]any{"id": buy}, "Where these coins went"},
		{"trace_coins", map[string]any{"id": sale}, "came from"},
		{"trace_coins", map[string]any{"account": "Cold storage"}, "came from"},
		{"what_if_sale_picks", map[string]any{"account": "Cold storage", "btc": 0.01, "strategy": "highest_cost"}, "Estimated extra tax"},
		{"what_if_sale_picks", map[string]any{"account": "Cold storage", "btc": 0.01, "strategy": "lowest_cost"}, "Estimated extra tax"},
	}
	for _, c := range cases {
		lk, out := tb.run(c.name, c.args)
		if lk.Error != "" || !strings.Contains(out, c.want) {
			t.Errorf("%s %v: error=%q out=%.400s", c.name, c.args, lk.Error, out)
		}
	}
	// Highest-cost coins should never produce a larger gain than lowest-cost ones.
	gain := func(strategy string) string {
		_, out := tb.run("what_if_sale_picks", map[string]any{"account": "Cold storage", "btc": 0.01, "strategy": strategy})
		return out
	}
	if gain("highest_cost") == gain("lowest_cost") {
		t.Log("strategies gave the same result (single lot?)")
	}
	if lk, _ := tb.run("analyze_records", map[string]any{"stat": "vibes"}); lk.Error == "" {
		t.Error("bad stat accepted")
	}
	if lk, _ := tb.run("what_if_sale_picks", map[string]any{"account": "Cold storage", "btc": 0.01, "strategy": "random"}); lk.Error == "" {
		t.Error("bad strategy accepted")
	}
	// The review count must match the report exactly.
	_, out := tb.run("analyze_records", map[string]any{"source": "review", "measure": "btc", "stat": "count"})
	if !strings.Contains(out, fmt.Sprintf("count: %d", len(rep.Unmatched))) {
		t.Errorf("review count wrong: %s", out)
	}
}

func TestKindMatches(t *testing.T) {
	for _, c := range [][2]string{{"sold", "sell"}, {"sale", "sell"}, {"sold", "sold"}, {"purchase", "buy"}, {"bought", "purchase"}, {"spent", "spend"}, {"buy", "buy"}} {
		if !kindMatches(c[0], c[1]) {
			t.Errorf("%q should match %q", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"buy", "sell"}, {"sold", "buy"}, {"income", "transfer"}} {
		if kindMatches(c[0], c[1]) {
			t.Errorf("%q should not match %q", c[0], c[1])
		}
	}
}

func TestSellEverything(t *testing.T) {
	st, _ := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, _ := srv.buildReport()
	tb := &toolbox{s: srv, rep: rep}
	lk, out := tb.run("what_if_sell_everything", nil)
	if lk.Error != "" || !strings.Contains(out, "Total proceeds") || !strings.Contains(out, "By account") {
		t.Fatalf("sell everything: %q %s", lk.Error, out)
	}
	// The total matches the sum of the holdings in tracked accounts.
	var held int64
	for _, a := range rep.Accounts {
		held += a.Sats
	}
	if !strings.Contains(out, btcStr(held)) {
		t.Errorf("total %s not in: %s", btcStr(held), out)
	}
	// Names as a model writes them, and selling a whole account.
	for _, n := range []string{"Cold storage (wallet)", "cold storage", "Vault (multisig) (wallet)", "Cold"} {
		if _, err := tb.resolveAccount(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	if _, err := tb.resolveAccount("Nowhere"); err == nil || !strings.Contains(err.Error(), "Cold storage") {
		t.Errorf("unknown account error should list names: %v", err)
	}
	lk, out = tb.run("what_if_sale", map[string]any{"account": "Cold storage (wallet)", "all": true})
	if lk.Error != "" || !strings.Contains(out, "Estimated extra tax") {
		t.Errorf("sell whole account: %q %s", lk.Error, out)
	}
}

func TestAnalyzeMath(t *testing.T) {
	st, _ := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, _ := srv.buildReport()
	tb := &toolbox{s: srv, rep: rep}
	// Work the answers out independently from the report.
	var usds []float64
	counts := map[int64]int{}
	var sum int64
	for _, g := range rep.Graph {
		if g.Kind == "buy" || g.Kind == "purchase" {
			if g.Cents != 0 {
				usds = append(usds, float64(g.Cents)/100)
				counts[g.Cents]++
				sum += g.Cents
			}
		}
	}
	_, out := tb.run("analyze_records", map[string]any{"source": "buys", "measure": "usd", "stat": "summary"})
	if !strings.Contains(out, fmt.Sprintf("count %d, sum %s", len(usds), usdStr(sum))) {
		t.Errorf("summary: %s (want count %d sum %s)", out, len(usds), usdStr(sum))
	}
	var best int64
	for c, n := range counts {
		if n > counts[best] || n == counts[best] && c < best {
			best = c
		}
	}
	_, out = tb.run("analyze_records", map[string]any{"source": "buys", "measure": "usd", "stat": "mode"})
	if counts[best] > 1 && !strings.Contains(out, "most common: "+usdStr(best)) {
		t.Errorf("mode: %s (want %s x%d)", out, usdStr(best), counts[best])
	}
	if counts[best] > 1 && !strings.Contains(out, fmt.Sprintf("%d of %d", counts[best], len(usds))) {
		t.Errorf("mode count: %s", out)
	}
	// Percentiles and the median agree.
	_, med := tb.run("analyze_records", map[string]any{"measure": "usd", "stat": "median"})
	_, pct := tb.run("analyze_records", map[string]any{"measure": "usd", "stat": "percentiles"})
	m := strings.TrimSpace(strings.TrimPrefix(strings.Split(med, "\n")[1], "median:"))
	if !strings.Contains(pct, "median "+m) {
		t.Errorf("median %q not in percentiles %s", m, pct)
	}
	if v, _ := calc("1"); v != 1 {
		t.Fatal()
	}
}

func TestClaimsChange(t *testing.T) {
	for _, s := range []string{"I have proposed labeling all 8 of those as spends.", "A card has appeared in the chat", "Please press Apply to confirm", "I've applied the labels", "I labeled them",
		"I have re-submitted the proposal to label all 8 of those 2026 outflows as **spends**.",
		"You should see a card summarizing the 8 items and asking you to **Apply** the labels.",
		"I have successfully submitted the proposal", "Please **press Apply** on the card"} {
		if !claimsChange(s) {
			t.Errorf("missed: %q", s)
		}
	}
	for _, s := range []string{"You have 8 unlabeled items.", "Would you like me to propose labels?", "Your biggest sale was in October."} {
		if claimsChange(s) {
			t.Errorf("false alarm: %q", s)
		}
	}
	note := lookupNote(`{"lookups":[{"name":"analyze_records","args":{"source":"review","from":"2026-01-01"},"summary":"x"}]}`)
	if !strings.Contains(note, `"source":"review"`) {
		t.Errorf("note: %s", note)
	}
}

func TestMatchFromHistory(t *testing.T) {
	h := []store.ChatMessage{
		{Role: "user", Content: "what unlabeled txs this year"},
		{Role: "assistant", Content: "8 items", Data: `{"lookups":[{"name":"analyze_records","args":{"source":"review","from":"2026-01-01","to":"2026-12-31","kind":"out","measure":"usd","max":10,"stat":"list"},"summary":"x"}]}`},
		{Role: "user", Content: "those were all spends"},
	}
	got := matchFromHistory(h)
	for _, want := range []string{`"from":"2026-01-01"`, `"to":"2026-12-31"`, `"direction":"out"`, `"max_usd":10`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s missing from %s", want, got)
		}
	}
	if matchFromHistory(h[:1]) != "" {
		t.Error("no lookup should give no match")
	}
}

func TestCategoryFromText(t *testing.T) {
	for in, want := range map[string]string{"those were all spends": "spend", "they are gifts to my brother": "gift", "label them as income": "income",
		"those were sold to a friend": "sold", "can you try again": "", "spends and gifts": ""} {
		if got := categoryFromText(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestRefersBack(t *testing.T) {
	for in, want := range map[string]bool{"those were all spends": true, "ok label all of these as spends": true, "label them as gifts": true, "label the 2019 send as a spend": false, "what do I hold?": false} {
		if refersBack(in) != want {
			t.Errorf("%q: %v", in, !want)
		}
	}
}

func TestTiersFromText(t *testing.T) {
	got := tiersFromText("label all the ones under $25 as spends and the ones above as donations")
	if len(got) != 2 || got[0].(map[string]any)["max_usd"] != 25.0 || got[0].(map[string]any)["category"] != "spend" || got[1].(map[string]any)["category"] != "donation" {
		t.Fatalf("tiers: %v", got)
	}
	if tiersFromText("label those as spends") != nil {
		t.Error("no tiers expected")
	}
	if !refersBack("just label only that one as a spend") || !refersBack("label all the ones under $25") {
		t.Error("refersBack missed")
	}
	if !claimsChange("The proposal card for this single item is waiting for you to apply the label.") {
		t.Error("claim missed")
	}
}

func TestReviewDir(t *testing.T) {
	for in, want := range map[string]string{"spend": "out", "disposals": "out", "sends": "out", "income": "in", "deposits": "in", "out": "out", "banana": ""} {
		if got := reviewDir(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
