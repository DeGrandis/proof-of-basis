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
