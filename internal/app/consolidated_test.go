package app

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/DeGrandis/proof-of-basis/internal/demo"
)

func demoToolbox(t *testing.T) (*toolbox, *Report, func(method, path, body string) (int, string)) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, err := srv.buildReport()
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, string) {
		w := call(h, method, path, body)
		return w.Code, w.Body.String()
	}
	return &toolbox{s: srv, rep: rep, chatID: 3}, rep, do
}

func TestToolSetIsSmall(t *testing.T) {
	specs := toolSpecs()
	names := map[string]bool{}
	size := 0
	for _, s := range specs {
		names[s.Function.Name] = true
		b, _ := json.Marshal(s)
		size += len(b)
	}
	for _, want := range []string{"analyze_records", "get_record", "accounts", "get_tax_year", "simulate_sale", "price", "calculate", "data_health", "label_items", "link_transfer", "protect_lot", "undo_action"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
	if len(specs) > 12 {
		t.Errorf("%d tools offered, want at most 12", len(specs))
	}
	t.Logf("%d tools, %d chars (~%d tokens)", len(specs), size, size/4)
}

func TestConsolidatedTools(t *testing.T) {
	tb, rep, do := demoToolbox(t)
	var sale, buy string
	for _, g := range rep.Graph {
		if g.Kind == "sell" && sale == "" {
			sale = g.Key
		}
		if g.Kind == "buy" && buy == "" {
			buy = g.Key
		}
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"get_record", map[string]any{"id": sale}, "came from"},
		{"get_record", map[string]any{"id": buy}, "Where these coins went"},
		{"get_record", map[string]any{"id": rep.Unmatched[0].Key}, "Waiting in Review"},
		{"accounts", map[string]any{}, "Coldcard"},
		{"accounts", map[string]any{"name": "coldcard (wallet)"}, "Purchase lots"},
		{"accounts", map[string]any{"name": "Coldcard", "sources": true}, "came from"},
		{"accounts", map[string]any{"date": "2023-12-31"}, "By account"},
		{"simulate_sale", map[string]any{"account": "all", "all": true}, "Total proceeds"},
		{"simulate_sale", map[string]any{"account": "Coldcard", "all": true}, "Estimated extra tax"},
		{"simulate_sale", map[string]any{"account": "Coldcard", "btc": 0.01, "strategy": "highest_cost"}, "Estimated extra tax"},
		{"price", map[string]any{"date": "2023-06-15"}, "BTC closed"},
		{"price", map[string]any{"from": "2023-01-01", "to": "2023-12-31"}, "high"},
		{"analyze_records", map[string]any{"source": "records", "stat": "list", "kind": "sold", "account": "River", "top_n": float64(3)}, "records match"},
		{"analyze_records", map[string]any{"source": "lots", "measure": "days_to_long_term", "max": float64(400), "stat": "list"}, "long-term on"},
		{"analyze_records", map[string]any{"source": "lots", "measure": "gain", "max": float64(0), "stat": "list"}, "lot "},
		{"analyze_records", map[string]any{"source": "income", "stat": "list"}, "worth"},
		{"analyze_records", map[string]any{"source": "review", "stat": "list", "account": "Phone wallet"}, "Phone wallet"},
	}
	for _, c := range cases {
		lk, out := tb.run(c.name, c.args)
		if lk.Error != "" || !strings.Contains(out, c.want) {
			t.Errorf("%s %v: error=%q out=%.300s", c.name, c.args, lk.Error, out)
		}
	}
	// Partial sales need an account: without one, each is compared.
	if lk, out := tb.run("simulate_sale", map[string]any{"account": "all", "btc": 0.1}); lk.Error != "" || !strings.Contains(out, "needs one account") {
		t.Errorf("partial sale across all accounts: %s %s", lk.Error, out)
	}

	// Working backwards: the verified gain stays under the limit.
	gainRe := regexp.MustCompile(`gain (-?\$[\d,]+\.\d\d) \(short-term`)
	for _, limit := range []float64{0, 500, 2000} {
		_, out := tb.run("simulate_sale", map[string]any{"target_gain": limit})
		m := gainRe.FindStringSubmatch(out)
		if m == nil {
			if !strings.Contains(out, "Nothing can be sold") {
				t.Fatalf("target_gain %v: %s", limit, out)
			}
			continue
		}
		g, _ := strconv.ParseFloat(strings.NewReplacer("$", "", ",", "").Replace(m[1]), 64)
		if g > limit+0.01 {
			t.Errorf("target_gain %v gave gain %v: %s", limit, g, out)
		}
	}
	if _, out := tb.run("simulate_sale", map[string]any{"target_tax": 100, "account": "Phone wallet"}); !strings.Contains(out, "extra") {
		t.Errorf("target_tax: %s", out)
	}
	// Exactly at or under the limit, to the cent, for many limits.
	for _, limit := range []float64{37, 123.45, 999.99, 2000, 4321} {
		_, out := tb.run("simulate_sale", map[string]any{"target_gain": limit})
		if m := gainRe.FindStringSubmatch(out); m != nil {
			g, _ := strconv.ParseFloat(strings.NewReplacer("$", "", ",", "").Replace(m[1]), 64)
			if g > limit {
				t.Errorf("target_gain %v gave %v", limit, g)
			}
		}
	}

	// Export: a CSV with every matching row, behind a login-protected link.
	lk, out := tb.run("analyze_records", map[string]any{"source": "buys", "stat": "count", "export": true})
	if lk.File == nil || !strings.HasPrefix(lk.File.URL, "/api/ai/export/") || !strings.HasSuffix(lk.File.Name, ".csv") || !strings.Contains(out, "CSV") {
		t.Fatalf("export: %+v %s", lk, out)
	}
	code, body := do("GET", lk.File.URL, "")
	buys := 0
	for _, g := range rep.Graph {
		if (g.Kind == "buy" || g.Kind == "purchase") && g.Cents != 0 {
			buys++
		}
	}
	if code != 200 || strings.Count(body, "\n") != buys+1 || !strings.HasPrefix(body, "date,account") {
		t.Errorf("export download: %d, %d lines, want %d", code, strings.Count(body, "\n"), buys+1)
	}
	if code, _ := do("GET", "/api/ai/export/nope", ""); code != 404 {
		t.Errorf("unknown export: %d", code)
	}
	// Chart: one bar per group.
	lk, _ = tb.run("analyze_records", map[string]any{"source": "buys", "measure": "usd", "stat": "sum", "group_by": "year", "chart": true})
	if lk.Chart == nil || len(lk.Chart.Labels) < 3 || len(lk.Chart.Labels) != len(lk.Chart.Values) {
		t.Fatalf("chart: %+v", lk.Chart)
	}
	lk, _ = tb.run("analyze_records", map[string]any{"measure": "weekday", "stat": "count", "chart": true})
	if lk.Chart == nil || len(lk.Chart.Labels) != 7 {
		t.Errorf("weekday chart: %+v", lk.Chart)
	}
}

func TestBulkLabelAndUndoTool(t *testing.T) {
	tb, rep, do := demoToolbox(t)
	var hot int
	for _, it := range rep.Unmatched {
		if it.Account == "Phone wallet" && it.Dir == "out" {
			hot++
		}
	}
	lk, msg := tb.run("label_items", map[string]any{"match": map[string]any{"account": "Phone wallet", "direction": "out"}, "category": "spend"})
	if lk.Error != "" || !strings.Contains(msg, "must press Apply") {
		t.Fatalf("bulk: %q %s", lk.Error, msg)
	}
	a := lastAction(t, tb.s.st)
	var cs []change
	json.Unmarshal(a.Changes, &cs)
	if len(cs) != hot {
		t.Fatalf("bulk label covered %d items, want %d", len(cs), hot)
	}
	if code, _ := do("POST", "/api/actions/"+strconv.FormatInt(a.ID, 10)+"/apply", ""); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	if lk, _ := tb.run("label_items", map[string]any{"match": map[string]any{"account": "Nowhere"}, "category": "spend"}); lk.Error == "" {
		t.Error("bad account accepted")
	}
	// "Undo that" right after a proposal just withdraws it.
	tb.run("protect_lot", map[string]any{"id": rep.OpenLots[0].ID})
	p := lastAction(t, tb.s.st)
	_, msg = tb.run("undo_action", nil)
	if q, _ := tb.s.st.Action(p.ID); q.Status != "dismissed" || !strings.Contains(msg, "Withdrew") {
		t.Fatalf("pending not withdrawn: %s %s", q.Status, msg)
	}
	// "Undo that": offers the change with its Undo button...
	_, msg = tb.run("undo_action", nil)
	if !strings.Contains(msg, "Undo button") || len(tb.actions) == 0 || tb.actions[len(tb.actions)-1] != a.ID {
		t.Fatalf("undo offer: %s %v", msg, tb.actions)
	}
	if b, _ := tb.s.st.Action(a.ID); b.Status != "done" {
		t.Fatal("undo happened without confirmation")
	}
	// ...or does it at once when confirmation is off.
	tb.s.st.SetSetting("aiAutoApply", "true")
	_, msg = tb.run("undo_action", map[string]any{"id": float64(a.ID)})
	if b, _ := tb.s.st.Action(a.ID); b.Status != "undone" || !strings.Contains(msg, "Undone") {
		t.Fatalf("auto undo: %s %s", b.Status, msg)
	}
	if lk, _ := tb.run("undo_action", map[string]any{"id": float64(a.ID)}); lk.Error == "" {
		t.Error("undoing twice accepted")
	}
}

func TestAnalyzeAliases(t *testing.T) {
	tb, rep, _ := demoToolbox(t)
	want := 0
	for _, it := range rep.Unmatched {
		if it.Dir == "out" && it.Value <= 5000 {
			want++
		}
	}
	for _, args := range []map[string]any{
		{"source": "review", "stat": "count", "kind": "out", "max": 50.0, "measure": "usd"},
		{"source": "review", "stat": "count", "kind": "spend", "max_usd": 50.0},
		{"source": "review", "stat": "count", "match": map[string]any{"max_usd": 50.0, "direction": "out"}},
	} {
		_, out := tb.run("analyze_records", args)
		if !strings.Contains(out, "count: "+strconv.Itoa(want)) {
			t.Errorf("%v: want %d, got %s", args, want, out)
		}
	}
}
