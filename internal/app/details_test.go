package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"btctax/internal/importers"
)

// Income types, the Form 1040 answer, and what gifts and donations need.
// Invented data.
func TestYearDocuments(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.PutPrices(map[string]int64{"2025-02-01": 100_000_00, "2025-03-01": 100_000_00, "2025-04-01": 100_000_00}, "test")
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2023-01-10"), Sats: 100_000_000, FiatCents: 20_000_00, UID: "b1"},
		{Kind: importers.Income, Time: at("2025-02-01"), Sats: 10_000, FiatCents: 10_00, Note: "Coinbase Earn reward", UID: "i1"},
		// 0.25 BTC ($25,000) to Alex, and 0.1 BTC ($10,000) to a charity.
		{Kind: importers.Withdrawal, Time: at("2025-03-01"), Sats: 25_000_000, Address: "bc1qgift", UID: "g1"},
		{Kind: importers.Withdrawal, Time: at("2025-04-01"), Sats: 10_000_000, Address: "bc1qcharity", UID: "d1"},
	}, nil)
	call(h, "POST", "/api/labels", `{"Key":"ex:3","Category":"gift","Recipient":"Alex"}`)
	call(h, "POST", "/api/labels", `{"Key":"ex:4","Category":"donation"}`)
	if w := call(h, "POST", "/api/labels/ex:4/details", `{"Recipient":"Food Bank"}`); w.Code != 200 {
		t.Fatalf("recipient: %d %s", w.Code, w.Body.String())
	}

	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	var y2025 YearSummary
	for _, y := range rep.Years {
		if y.Year == 2025 {
			y2025 = y
		}
	}
	if y2025.IncomeByType["rewards"] != 10_00 {
		t.Errorf("income by type %v", y2025.IncomeByType)
	}
	if y2025.DigitalAsset.Answer != "Yes" || len(y2025.DigitalAsset.Reasons) != 1 || len(y2025.DigitalAsset.Ask) != 1 {
		t.Errorf("1040 answer %+v", y2025.DigitalAsset)
	}
	gifts := map[string]GiftRow{}
	for _, g := range rep.Gifts {
		gifts[g.Category] = g
	}
	g := gifts["gift"]
	if g.Recipient != "Alex" || g.Value != 25_000_00 || g.Term != "long" || !strings.Contains(strings.Join(g.Flags, " "), "Form 709") {
		t.Errorf("gift %+v", g)
	}
	d := gifts["donation"]
	// Held over a year: the deduction is the market value.
	if d.Recipient != "Food Bank" || d.Deduction != 10_000_00 || !strings.Contains(strings.Join(d.Flags, " "), "Form 8283") || !strings.Contains(strings.Join(d.Flags, " "), "acknowledgment") {
		t.Errorf("donation %+v", d)
	}

	// The reward was really staking: change its type, not its value.
	if w := call(h, "POST", "/api/labels/ex:2/details", `{"IncomeType":"staking"}`); w.Code != 200 {
		t.Fatalf("type: %d %s", w.Code, w.Body.String())
	}
	var rep2 Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep2)
	for _, y := range rep2.Years {
		if y.Year == 2025 && (y.IncomeByType["staking"] != 10_00 || y.IncomeByType["rewards"] != 0 || y.Income != 10_00) {
			t.Errorf("after retyping: %+v", y)
		}
	}
	for _, e := range rep2.Graph {
		if e.Key == "ex:2" && (e.IncomeType != "staking" || e.Label != "") {
			t.Errorf("graph event %+v: a type alone doesn't mark the value as set", e)
		}
	}
	if w := call(h, "POST", "/api/labels/ex:2/details", `{"IncomeType":"lottery"}`); w.Code != 400 {
		t.Errorf("unknown type accepted: %d", w.Code)
	}

	// A year with only purchases: No.
	for _, y := range rep2.Years {
		if y.Year == 2023 && y.DigitalAsset.Answer != "No" {
			t.Errorf("2023 %+v", y.DigitalAsset)
		}
	}
}

// The January 2025 split: the default, a purchase assigned to its own
// account, and a confirmed record that notices later changes.
func TestAllocationSplit(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	rv, _ := st.ExchangeAccount("River", "river")
	st.AddImport(rv, "river", "r.csv", "r", []importers.Event{{Kind: importers.Buy, Time: at("2020-01-01"), Sats: 100_000, FiatCents: 100_00, UID: "r1"}}, nil)
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{{Kind: importers.Buy, Time: at("2023-01-01"), Sats: 100_000, FiatCents: 500_00, UID: "c1"}}, nil)
	report := func() Report {
		var r Report
		json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &r)
		return r
	}
	owner := func(r Report) map[string]string {
		m := map[string]string{}
		for _, a := range r.Allocation.Rows {
			m[a.BoughtOn] = a.Account
		}
		return m
	}
	// Default: Coinbase sorts first and takes River's older coins.
	if o := owner(report()); o["River"] != "Coinbase" {
		t.Fatalf("default split %v", o)
	}
	if w := call(h, "POST", "/api/allocation/confirm", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if r := report(); r.Allocation.Confirmed == "" || r.Allocation.Changed {
		t.Errorf("confirmed %+v", r.Allocation)
	}
	// Each account keeps its own purchase once one is assigned.
	call(h, "POST", "/api/allocation", `{"OriginID":"ex:1","Account":"River"}`)
	r := report()
	if o := owner(r); o["River"] != "River" || o["Coinbase"] != "Coinbase" {
		t.Errorf("assigned split %v", o)
	}
	if !r.Allocation.Changed {
		t.Error("a change after confirming isn't flagged")
	}
}

// Plan a spend, record the coins chosen, and the sale that follows uses
// them; a choice recorded after its sale is never applied.
func TestRecordedIdentification(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	now := time.Now().UTC()
	st.PutPrices(map[string]int64{now.Format("2006-01-02"): 100_000_00, now.AddDate(0, 0, 2).Format("2006-01-02"): 100_000_00}, "test")
	rv, _ := st.ExchangeAccount("River", "river")
	st.AddImport(rv, "river", "r.csv", "r", []importers.Event{
		{Kind: importers.Buy, Time: at("2023-01-01"), Sats: 1_000_000, FiatCents: 200_00, UID: "cheap"},
		{Kind: importers.Buy, Time: at("2024-03-01"), Sats: 1_000_000, FiatCents: 700_00, UID: "dear"},
		{Kind: importers.Sell, Time: at("2024-06-01"), Sats: 100_000, FiatCents: 70_00, UID: "past"},
	}, nil)
	// The planner: highest cost first picks the 2024 purchase.
	var plan struct {
		Lots []struct {
			OriginID string `json:"originId"`
			Sats     int64  `json:"sats"`
		} `json:"lots"`
	}
	json.Unmarshal(call(h, "POST", "/api/whatif", `{"account":"River","btc":0.005,"strategy":"highest_cost"}`).Body.Bytes(), &plan)
	if len(plan.Lots) != 1 || plan.Lots[0].OriginID != "ex:2" {
		t.Fatalf("plan %+v", plan)
	}
	body, _ := json.Marshal(map[string]any{"Account": "River", "Strategy": "highest_cost",
		"Picks": []map[string]any{{"originId": plan.Lots[0].OriginID, "sats": plan.Lots[0].Sats}}})
	if w := call(h, "POST", "/api/identifications", string(body)); w.Code != 200 {
		t.Fatalf("record: %d %s", w.Code, w.Body.String())
	}
	// The sale happens two days later.
	st.AddImport(rv, "river", "r2.csv", "r2", []importers.Event{
		{Kind: importers.Sell, Time: now.AddDate(0, 0, 2), Sats: 500_000, FiatCents: 500_00, UID: "planned"},
	}, nil)
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	for _, d := range rep.Disposals {
		if d.OpID == "ex:4" && d.OriginID != "ex:2" {
			t.Errorf("the planned sale used %s, not the coins chosen", d.OriginID)
		}
	}
	if len(rep.Identifications) != 1 || rep.Identifications[0].Status != "applied" || rep.Identifications[0].AppliedTo != "ex:4" {
		t.Fatalf("identification %+v", rep.Identifications)
	}
	// Tied to a sale that happened before it was recorded: not used.
	call(h, "POST", "/api/identifications/1", `{"AppliesTo":"ex:3"}`)
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	if v := rep.Identifications[0]; v.Status != "late" || v.Warning == "" {
		t.Errorf("late identification %+v", v)
	}
	for _, d := range rep.Disposals {
		if d.OpID == "ex:3" && d.OriginID != "ex:1" {
			t.Errorf("a late choice changed a past sale: %+v", d)
		}
	}
}

// A wrong automatic match can be undone: the pair goes back to Review
// and isn't matched again until allowed.
func TestUnlinkMatch(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	rv, _ := st.ExchangeAccount("River", "river")
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2024-01-01"), Sats: 500_000, FiatCents: 200_00, UID: "b"},
		{Kind: importers.Withdrawal, Time: at("2024-02-01"), Sats: 300_000, UID: "w"},
	}, nil)
	st.AddImport(rv, "river", "r.csv", "r", []importers.Event{{Kind: importers.Deposit, Time: at("2024-02-01"), Sats: 300_000, UID: "d"}}, nil)
	report := func() Report {
		var r Report
		json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &r)
		return r
	}
	if r := report(); len(r.Links) != 1 || r.Links[0].From != "ex:2" || r.Links[0].To != "ex:3" {
		t.Fatalf("links %+v", r.Links)
	}
	call(h, "POST", "/api/unlink", `{"From":"ex:2","To":"ex:3"}`)
	if r := report(); len(r.Links) != 0 || len(r.Unmatched) != 2 || len(r.Unlinked) != 1 {
		t.Errorf("after unlinking: links %+v, review %d, unlinked %v", r.Links, len(r.Unmatched), r.Unlinked)
	}
	call(h, "POST", "/api/unlink", `{"From":"ex:2","To":"ex:3","Allow":true}`)
	if r := report(); len(r.Links) != 1 {
		t.Errorf("allowed again: %+v", r.Links)
	}
}

// The readiness check names what's missing and turns ready once fixed.
func TestReadiness(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2025-01-10"), Sats: 100_000, FiatCents: 100_00, UID: "b"},
		{Kind: importers.Sell, Time: at("2025-03-01"), Sats: 150_000, FiatCents: 160_00, UID: "s"}, // 50,000 never bought
		{Kind: importers.Withdrawal, Time: at("2025-04-01"), Sats: 10_000, Address: "bc1qsomewhere", UID: "w"},
		{Kind: importers.Income, Time: at("2025-05-01"), Sats: 1_000, FiatCents: 1_00, UID: "i"},
	}, nil)
	get := func() Readiness {
		var r Readiness
		json.Unmarshal(call(h, "GET", "/api/readiness?year=2025", "").Body.Bytes(), &r)
		return r
	}
	status := func(r Readiness) map[string]string {
		m := map[string]string{}
		for _, c := range r.Checks {
			m[c.ID] = c.Status
		}
		return m
	}
	r := get()
	s := status(r)
	if r.Ready || s["basis"] != "bad" || s["review"] != "bad" || s["1099da"] != "warn" || s["income-type"] != "warn" || r.KnownBasis != 6666 {
		t.Errorf("before: ready %v known %d checks %v", r.Ready, r.KnownBasis, s)
	}
	// Fix it: the missing purchase, the withdrawal labeled, the income typed.
	st.AddImport(cb, "coinbase", "c2.csv", "c2", []importers.Event{{Kind: importers.Buy, Time: at("2025-01-20"), Sats: 60_000, FiatCents: 60_00, UID: "b2"}}, nil)
	call(h, "POST", "/api/labels", `{"Key":"ex:3","Category":"spend","FiatCents":1000}`)
	call(h, "POST", "/api/labels/ex:4/details", `{"IncomeType":"staking"}`)
	r = get()
	s = status(r)
	if !r.Ready || s["basis"] != "ok" || s["review"] != "ok" || s["income-type"] != "" {
		t.Errorf("after: ready %v checks %+v", r.Ready, r.Checks)
	}
}
