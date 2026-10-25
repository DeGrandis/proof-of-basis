package app

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"btctax/internal/demo"
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
	tb := &toolbox{s: srv, rep: rep, chatID: 9, historyMatch: `{"direction":"out","account":"Hot wallet"}`}
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
