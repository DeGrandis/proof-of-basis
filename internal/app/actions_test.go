package app

import (
	"btctax/internal/trace"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"btctax/internal/demo"
	"btctax/internal/importers"
	"btctax/internal/store"
)

func lastAction(t *testing.T, st *store.Store) store.Action {
	t.Helper()
	list, err := st.Actions("", 1)
	if err != nil || len(list) == 0 {
		t.Fatalf("no action logged: %v", err)
	}
	return list[0]
}

func TestUndoLabel(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	key := rep.Unmatched[0].Key
	before := len(rep.Unmatched)
	if w := call(h, "POST", "/api/labels", `{"Key":"`+key+`","Category":"spend"}`); w.Code != 200 {
		t.Fatalf("label: %d %s", w.Code, w.Body.String())
	}
	a := lastAction(t, st)
	if a.Actor != "user" || a.Kind != "label" || a.Status != "done" {
		t.Fatalf("logged: %+v", a)
	}
	id := strconv.FormatInt(a.ID, 10)
	// The list reads like a sentence.
	if w := call(h, "GET", "/api/actions", ""); !strings.Contains(w.Body.String(), "labeled as a spend") {
		t.Fatalf("list: %s", w.Body.String())
	}
	if w := call(h, "POST", "/api/actions/"+id+"/undo", ""); w.Code != 200 {
		t.Fatalf("undo: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	if len(rep.Unmatched) != before || st.LabelRaw(key) != "" {
		t.Fatalf("undo didn't remove the label")
	}
	if w := call(h, "POST", "/api/actions/"+id+"/undo", ""); w.Code != 400 {
		t.Fatalf("double undo: %d", w.Code)
	}
	if w := call(h, "POST", "/api/actions/"+id+"/redo", ""); w.Code != 200 || st.LabelRaw(key) == "" {
		t.Fatalf("redo: %d", w.Code)
	}
	// A later change blocks undo unless forced.
	call(h, "POST", "/api/labels", `{"Key":"`+key+`","Category":"gift"}`)
	if w := call(h, "POST", "/api/actions/"+id+"/undo", ""); w.Code != 409 {
		t.Fatalf("conflict not caught: %d", w.Code)
	}
	if w := call(h, "POST", "/api/actions/"+id+"/undo", `{"force":true}`); w.Code != 200 || st.LabelRaw(key) != "" {
		t.Fatalf("forced undo: %d", w.Code)
	}
	// Saving the same label twice logs once.
	n0, _ := st.Actions("", 1000)
	call(h, "POST", "/api/labels", `{"Key":"`+key+`","Category":"lost"}`)
	call(h, "POST", "/api/labels", `{"Key":"`+key+`","Category":"lost"}`)
	n1, _ := st.Actions("", 1000)
	if len(n1)-len(n0) != 1 {
		t.Errorf("no-op change was logged: %d new", len(n1)-len(n0))
	}
}

func TestUndoDeletesAndAdds(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		ev, _ := st.ExchangeEvents()
		return len(ev)
	}
	ims, _ := st.Imports()
	n := count()
	if w := call(h, "DELETE", "/api/imports/"+strconv.FormatInt(ims[0].ID, 10), ""); w.Code != 200 {
		t.Fatalf("delete import: %d", w.Code)
	}
	if count() >= n {
		t.Fatal("import not deleted")
	}
	a := lastAction(t, st)
	if !strings.Contains(a.Summary, "Deleted import") {
		t.Fatalf("summary: %s", a.Summary)
	}
	if w := call(h, "POST", "/api/actions/"+strconv.FormatInt(a.ID, 10)+"/undo", ""); w.Code != 200 {
		t.Fatalf("undo delete: %d %s", w.Code, w.Body.String())
	}
	if count() != n {
		t.Fatalf("events after undo: %d, want %d", count(), n)
	}
	// Deleting a whole account and undoing it.
	accts, _ := st.Accounts()
	var wallet store.Account
	for _, a := range accts {
		if a.Kind == "wallet" {
			wallet = a
		}
	}
	wtx, _ := st.WalletTxs()
	call(h, "DELETE", "/api/accounts/"+strconv.FormatInt(wallet.ID, 10), "")
	a = lastAction(t, st)
	call(h, "POST", "/api/actions/"+strconv.FormatInt(a.ID, 10)+"/undo", "")
	back, err := st.Account(wallet.ID)
	wtx2, _ := st.WalletTxs()
	if err != nil || back.XPub != wallet.XPub || len(wtx2) != len(wtx) {
		t.Fatalf("account not restored: %v %d/%d", err, len(wtx2), len(wtx))
	}
	// Adding a wallet, then undoing it, removes it.
	if w := call(h, "POST", "/api/wallets", `{"name":"Test zpub","key":"`+zpub+`"}`); w.Code != 200 {
		t.Fatalf("add wallet: %d %s", w.Code, w.Body.String())
	}
	a = lastAction(t, st)
	call(h, "POST", "/api/actions/"+strconv.FormatInt(a.ID, 10)+"/undo", "")
	if _, ok := st.AccountByName("Test zpub"); ok {
		t.Fatal("added wallet survived undo")
	}
	call(h, "POST", "/api/actions/"+strconv.FormatInt(a.ID, 10)+"/redo", "")
	if _, ok := st.AccountByName("Test zpub"); !ok {
		t.Fatal("redo didn't bring the wallet back")
	}
}

func TestUndoSettingsAndNotes(t *testing.T) {
	st, h := newTest(t)
	call(h, "POST", "/api/settings", `{"rateShort":"30","method":"lifo"}`)
	a := lastAction(t, st)
	if a.Kind != "settings" {
		t.Fatalf("kind %s", a.Kind)
	}
	var cs []change
	json.Unmarshal(a.Changes, &cs)
	if len(cs) != 2 {
		t.Fatalf("changes: %s", a.Changes)
	}
	call(h, "POST", "/api/actions/"+strconv.FormatInt(a.ID, 10)+"/undo", "")
	if st.Setting("rateShort", "24") != "24" || st.Setting("method", "fifo") != "fifo" {
		t.Fatalf("settings not undone: %s %s", st.Setting("rateShort", "?"), st.Setting("method", "?"))
	}
	// The rune is never logged.
	call(h, "POST", "/api/settings", `{"clnRune":"secret-rune-value"}`)
	all, _ := st.Actions("", 100)
	for _, x := range all {
		if strings.Contains(string(x.Changes), "secret-rune-value") || strings.Contains(x.Summary, "secret-rune-value") {
			t.Fatal("rune ended up in the log")
		}
	}
	if lastAction(t, st).Status != "info" {
		t.Fatal("rune save not noted")
	}
	// Wipe clears the log.
	call(h, "POST", "/api/wipe", `{"confirm":"WIPE"}`)
	if all, _ := st.Actions("", 10); len(all) != 0 {
		t.Fatalf("wipe left %d actions", len(all))
	}
}

func TestAssistantProposals(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, _ := srv.buildReport()
	tb := &toolbox{s: srv, rep: rep, chatID: 7}
	var out, in string
	for _, it := range rep.Unmatched {
		if it.Dir == "out" && out == "" {
			out = it.Key
		}
	}
	key := rep.Unmatched[0].Key
	lk, msg := tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": key, "category": "spend", "note": "coffee"}}})
	if lk.Error != "" || !strings.Contains(msg, "must press Apply") || st.LabelRaw(key) != "" {
		t.Fatalf("proposal: %q %q label=%q", lk.Error, msg, st.LabelRaw(key))
	}
	a := lastAction(t, st)
	if a.Status != "pending" || a.Actor != "assistant" || a.ChatID != 7 {
		t.Fatalf("pending: %+v", a)
	}
	id := strconv.FormatInt(a.ID, 10)
	if w := call(h, "POST", "/api/actions/"+id+"/apply", ""); w.Code != 200 || !strings.Contains(st.LabelRaw(key), "coffee") {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	if w := call(h, "POST", "/api/actions/"+id+"/undo", ""); w.Code != 200 || st.LabelRaw(key) != "" {
		t.Fatalf("undo applied proposal: %d", w.Code)
	}
	// Asking twice in one chat gives one card.
	tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": key, "category": "gift"}}})
	n1, _ := st.Actions("assistant", 100)
	tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": key, "category": "gift"}}})
	n2, _ := st.Actions("assistant", 100)
	if len(n2) != len(n1) {
		t.Errorf("duplicate proposal created")
	}
	// A category that doesn't fit the direction is refused.
	if lk, _ := tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": out, "category": "income"}}}); lk.Error == "" {
		t.Error("income on an outflow accepted")
	}
	// Dismissed proposals change nothing.
	tb.run("protect_lot", map[string]any{"id": rep.OpenLots[0].ID})
	p := lastAction(t, st)
	call(h, "POST", "/api/actions/"+strconv.FormatInt(p.ID, 10)+"/dismiss", "")
	if srv.protected()[rep.OpenLots[0].OriginID] {
		t.Fatal("dismissed protection applied")
	}
	// With confirmation off, changes apply at once and can still be undone.
	st.SetSetting("aiAutoApply", "true")
	_, msg = tb.run("protect_lot", map[string]any{"id": rep.OpenLots[0].ID})
	if !strings.Contains(msg, "Done") || !srv.protected()[rep.OpenLots[0].OriginID] {
		t.Fatalf("auto-apply: %s", msg)
	}
	auto := lastAction(t, st)
	call(h, "POST", "/api/actions/"+strconv.FormatInt(auto.ID, 10)+"/undo", "")
	if srv.protected()[rep.OpenLots[0].OriginID] {
		t.Fatal("auto-applied change not undone")
	}
	// Filter by who did it.
	if w := call(h, "GET", "/api/actions?actor=assistant", ""); strings.Contains(w.Body.String(), `"actor":"user"`) {
		t.Fatal("actor filter leaked user actions")
	}
	_ = in
}

func TestBulkLabelEndpoint(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	var keys []string
	for _, it := range rep.Unmatched {
		if it.Dir == "out" && len(keys) < 5 {
			keys = append(keys, `"`+it.Key+`"`)
		}
	}
	w := call(h, "POST", "/api/labels/bulk", `{"keys":[`+strings.Join(keys, ",")+`],"category":"spend"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Labeled 5 Review items") {
		t.Fatalf("bulk: %d %s", w.Code, w.Body.String())
	}
	a := lastAction(t, st)
	var cs []change
	json.Unmarshal(a.Changes, &cs)
	if len(cs) != 5 || a.Actor != "user" {
		t.Fatalf("logged %d changes by %s", len(cs), a.Actor)
	}
	call(h, "POST", "/api/actions/"+strconv.FormatInt(a.ID, 10)+"/undo", "")
	var after Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &after)
	if len(after.Unmatched) != len(rep.Unmatched) {
		t.Fatalf("undo left %d of %d in Review", len(after.Unmatched), len(rep.Unmatched))
	}
	if w := call(h, "POST", "/api/labels/bulk", `{"keys":[`+keys[0]+`],"category":"income"}`); w.Code != 400 {
		t.Errorf("income on an outflow: %d", w.Code)
	}
}

func TestLabelIdFallback(t *testing.T) {
	st, _ := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, _ := srv.buildReport()
	tb := &toolbox{s: srv, rep: rep, chatID: 9, historyMatch: `{"direction":"out","account":"Hot wallet"}`, refersBack: true}
	want := 0
	for _, it := range rep.Unmatched {
		if it.Dir == "out" && it.Account == "Hot wallet" {
			want++
		}
	}
	lk, msg := tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": "2026-08-11", "category": "spend"}, map[string]any{"id": "made-up", "category": "spend"}}})
	if lk.Error != "" || !strings.Contains(msg, "must press Apply") {
		t.Fatalf("fallback: %q %s", lk.Error, msg)
	}
	var cs []change
	json.Unmarshal(lastAction(t, st).Changes, &cs)
	if len(cs) != want {
		t.Errorf("labeled %d, want %d", len(cs), want)
	}
}

func TestDescriptionTiersSupersede(t *testing.T) {
	st, _ := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, _ := srv.buildReport()
	// pick an item whose date is unique in Review
	var it trace.Item
	days := map[string]int{}
	for _, x := range rep.Unmatched {
		days[dayOf(x.Time)]++
	}
	for _, x := range rep.Unmatched {
		if days[dayOf(x.Time)] == 1 {
			it = x
			break
		}
	}
	tb := &toolbox{s: srv, rep: rep, chatID: 5}
	desc := dayOf(it.Time) + " " + it.Account + " out " + btcStr(it.Sats)
	lk, _ := tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": desc, "category": "spend"}}})
	if lk.Error != "" {
		t.Fatalf("by description: %s", lk.Error)
	}
	first := lastAction(t, st)
	var cs []change
	json.Unmarshal(first.Changes, &cs)
	if len(cs) != 1 || cs[0].Key != it.Key {
		t.Fatalf("labeled %v, want %s", cs, it.Key)
	}
	// A wrong id is NOT widened to a whole earlier search unless the user said "those".
	tb2 := &toolbox{s: srv, rep: rep, chatID: 6, historyMatch: `{"direction":"out"}`}
	if lk, _ := tb2.run("label_items", map[string]any{"items": []any{map[string]any{"id": "nonsense", "category": "spend"}}}); lk.Error == "" {
		t.Fatal("a bad id was silently widened")
	}
	// Tiers split by value; the new card replaces the earlier pending one.
	var keys []any
	for _, x := range rep.Unmatched {
		if x.Dir == "out" && len(keys) < 6 {
			keys = append(keys, map[string]any{"id": x.Key, "category": "spend"})
		}
	}
	lk, _ = tb.run("label_items", map[string]any{"items": keys, "tiers": []any{map[string]any{"max_usd": 25.0, "category": "spend"}, map[string]any{"category": "donation"}}})
	if lk.Error != "" {
		t.Fatalf("tiers: %s", lk.Error)
	}
	if a, _ := st.Action(first.ID); a.Status != "dismissed" {
		t.Errorf("earlier proposal not replaced: %s", a.Status)
	}
	second := lastAction(t, st)
	json.Unmarshal(second.Changes, &cs)
	for _, c := range cs {
		var l trace.Label
		json.Unmarshal(c.After, &l)
		var v int64
		for _, x := range rep.Unmatched {
			if x.Key == c.Key {
				v = x.Value
			}
		}
		want := trace.Category("spend")
		if v > 2500 {
			want = "donation"
		}
		if l.Category != want {
			t.Errorf("%s worth %d got %s, want %s", c.Key, v, l.Category, want)
		}
	}
}

func TestThoseNarrowed(t *testing.T) {
	st, _ := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil)
	rep, _ := srv.buildReport()
	var shown []string
	under := 0
	for _, it := range rep.Unmatched {
		if it.Dir == "out" && len(shown) < 20 {
			shown = append(shown, it.Key)
			if it.Value <= 1000 {
				under++
			}
		}
	}
	tb := &toolbox{s: srv, rep: rep, chatID: 4, refersBack: true, historyKeys: shown}
	// "Of those, label the ones under $10": only those, not all 20.
	lk, _ := tb.run("label_items", map[string]any{"match": map[string]any{"max_usd": 10.0}, "category": "spend"})
	if lk.Error != "" {
		t.Fatal(lk.Error)
	}
	var cs []change
	json.Unmarshal(lastAction(t, st).Changes, &cs)
	if len(cs) != under {
		t.Errorf("labeled %d, want %d of the shown 20", len(cs), under)
	}
	// "Label those": all of them.
	tb.run("label_items", map[string]any{"match": map[string]any{}, "category": "spend"})
	json.Unmarshal(lastAction(t, st).Changes, &cs)
	if len(cs) != len(shown) {
		t.Errorf("labeled %d, want all %d", len(cs), len(shown))
	}
}

// A wrong value on imported income, and a label already set, can both be
// corrected: from the page (a label with the value) and by the assistant
// (a proposal the user applies). Invented data.
func TestCorrectIncomeValueAndRelabel(t *testing.T) {
	st, h := newTest(t)
	acct, _ := st.ExchangeAccount("Coinbase", "coinbase")
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.AddImport(acct, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2024-01-01"), Sats: 1_000_000, FiatCents: 400_00, UID: "c1"},
		{Kind: importers.Income, Time: at("2024-02-01"), Sats: 10_000, FiatCents: 5_00, UID: "c2"},
		{Kind: importers.Withdrawal, Time: at("2024-03-01"), Sats: 20_000, Address: "bc1qsynthetic", UID: "c3"},
	}, nil)
	srv := New(st, nil)
	income := func() (int64, GraphEvent) {
		rep, _ := srv.buildReport()
		var v int64
		for _, in := range rep.Income {
			if in.Key == "ex:2" {
				v = in.Cents
			}
		}
		for _, g := range rep.Graph {
			if g.Key == "ex:2" {
				return v, g
			}
		}
		return v, GraphEvent{}
	}
	if v, _ := income(); v != 5_00 {
		t.Fatalf("imported value %d", v)
	}
	// The assistant proposes the corrected value; the user applies it.
	rep, _ := srv.buildReport()
	tb := &toolbox{s: srv, rep: rep, chatID: 1}
	if lk, _ := tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": "ex:2", "category": "income", "usd": 12.34}}}); lk.Error != "" {
		t.Fatalf("value proposal refused: %s", lk.Error)
	}
	call(h, "POST", "/api/actions/"+strconv.FormatInt(lastAction(t, st).ID, 10)+"/apply", "")
	if v, g := income(); v != 12_34 || g.Cents != 12_34 || g.Label != "income" {
		t.Errorf("after correcting: value %d graph %+v", v, g)
	}
	// Imported income stays income.
	if lk, _ := tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": "ex:2", "category": "spend"}}}); lk.Error == "" {
		t.Error("imported income relabeled as a spend")
	}
	// Removing the label goes back to the export's value.
	call(h, "DELETE", "/api/labels/ex:2", "")
	if v, _ := income(); v != 5_00 {
		t.Errorf("after reset: %d", v)
	}
	// A label already set can be changed by the assistant too.
	call(h, "POST", "/api/labels", `{"Key":"ex:3","Category":"spend","FiatCents":900}`)
	rep, _ = srv.buildReport()
	tb = &toolbox{s: srv, rep: rep, chatID: 2}
	if lk, _ := tb.run("label_items", map[string]any{"items": []any{map[string]any{"id": "ex:3", "category": "gift"}}}); lk.Error != "" {
		t.Fatalf("relabel refused: %s", lk.Error)
	}
	call(h, "POST", "/api/actions/"+strconv.FormatInt(lastAction(t, st).ID, 10)+"/apply", "")
	if !strings.Contains(st.LabelRaw("ex:3"), "gift") {
		t.Errorf("label now %s", st.LabelRaw("ex:3"))
	}
}
