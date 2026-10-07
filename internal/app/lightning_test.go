package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"btctax/internal/importers"
	"btctax/internal/lightning"
)

// fakeCLN answers like CLNRest with an invented node history: funded from
// an exchange withdrawal on-chain, a payment with a routing fee, a receipt
// matching a Strike Lightning withdrawal, one plain receipt, routing fees
// earned and an on-chain fee.
func fakeCLN(t *testing.T, rune string) *httptest.Server {
	day := func(s string) int64 { tm, _ := time.Parse("2006-01-02", s); return tm.Unix() }
	income := []lightning.RawIncome{
		{Account: "wallet", Tag: "deposit", Credit: 1_000_000_000, Timestamp: day("2025-02-01"), Outpoint: strings.Repeat("ab", 32) + ":0"},
		{Account: "wallet", Tag: "onchain_fee", Debit: 2_000_000, Timestamp: day("2025-02-02"), Txid: strings.Repeat("cd", 32)},
		{Account: "chan1", Tag: "invoice", Debit: 50_000_000, Timestamp: day("2025-03-01"), Description: "Coffee", PaymentID: "p1"},
		{Account: "chan1", Tag: "invoice_fee", Debit: 10_000, Timestamp: day("2025-03-01"), PaymentID: "p1"},
		{Account: "chan1", Tag: "invoice", Credit: 20_000_000, Timestamp: day("2025-03-05"), Description: "From Strike", PaymentID: "p2"},
		{Account: "chan1", Tag: "invoice", Credit: 7_000_000, Timestamp: day("2025-03-09"), Description: "Friend paid me back", PaymentID: "p3"},
		{Account: "chan1", Tag: "routed", Credit: 1_500_000, Timestamp: day("2025-04-01"), PaymentID: "p4"},
	}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Rune") != rune {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"code": 1501, "message": "Not authorized: rune check failed"})
			return
		}
		switch r.URL.Path {
		case "/v1/getinfo":
			json.NewEncoder(w).Encode(map[string]any{"alias": "testnode", "network": "regtest"})
		case "/v1/bkpr-listincome":
			json.NewEncoder(w).Encode(map[string]any{"income_events": income})
		case "/v1/bkpr-listbalances":
			json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{
				{"account": "wallet", "balances": []map[string]any{{"balance_msat": 400_000_000}}},
				{"account": "chan1", "peer_id": "02aa", "balances": []map[string]any{{"balance_msat": 576_490_000}}},
			}})
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestLightningNode(t *testing.T) {
	st, h := newTest(t)
	srv := fakeCLN(t, "secret-rune")
	defer srv.Close()

	// The node was funded from Coinbase on-chain; Strike paid it over Lightning.
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	sk, _ := st.ExchangeAccount("Strike", "strike")
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2025-01-01"), Sats: 2_000_000, FiatCents: 2_000_00, UID: "c1"},
		{Kind: importers.Withdrawal, Time: at("2025-02-01"), Sats: 1_000_000, Txid: strings.Repeat("ab", 32), UID: "c2"},
	}, nil)
	st.AddImport(sk, "strike", "s.csv", "s", []importers.Event{
		{Kind: importers.Buy, Time: at("2025-01-02"), Sats: 50_000, FiatCents: 50_00, UID: "s1"},
		{Kind: importers.Withdrawal, Time: at("2025-03-05"), Sats: 20_000, Address: "lnbc200u1synthetic", UID: "s2"},
	}, nil)
	st.PutPrices(map[string]int64{"2025-03-01": 100_000_00, "2025-03-09": 100_000_00, "2025-04-01": 100_000_00}, "import")

	call(h, "POST", "/api/settings", `{"clnUrl":"`+srv.URL+`","clnRune":"wrong"}`)
	if w := call(h, "POST", "/api/lightning/sync", ""); w.Code == 200 || strings.Contains(w.Body.String(), "wrong") {
		t.Fatalf("bad rune: %d %s", w.Code, w.Body.String())
	}
	call(h, "POST", "/api/settings", `{"clnRune":"secret-rune"}`)
	if w := call(h, "GET", "/api/settings", ""); strings.Contains(w.Body.String(), "secret-rune") || !strings.Contains(w.Body.String(), `"clnRuneSet":"true"`) {
		t.Fatalf("settings leak or flag missing: %s", w.Body.String())
	}
	w := call(h, "POST", "/api/lightning/sync", "")
	if w.Code != 200 {
		t.Fatalf("sync: %s", w.Body.String())
	}
	if call(h, "POST", "/api/lightning/sync", "").Code != 200 {
		t.Fatal("second sync failed")
	}

	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	if rep.Lightning == nil || rep.Lightning.Balance != 976_490 || len(rep.Lightning.Events) != 7 {
		t.Fatalf("lightning view %+v", rep.Lightning)
	}
	strikeLinked := false
	for _, l := range rep.Links {
		if l.From == "ex:4" && strings.HasPrefix(l.To, "ln:") {
			strikeLinked = true
		}
	}
	if !strikeLinked {
		t.Errorf("Strike Lightning withdrawal not matched to the node: %+v", rep.Links)
	}
	var paid, coffeeDisposal int64
	for _, d := range rep.Disposals {
		paid += d.Sats
		if d.Account == "Lightning node" {
			coffeeDisposal += d.Proceeds
		}
	}
	if paid != 50_010 || coffeeDisposal != 50_00 {
		t.Errorf("payment disposal: %d sats, $%d", paid, coffeeDisposal)
	}
	var income int64
	for _, y := range rep.Years {
		income += y.Income
	}
	if income != 7_00+1_50 { // the friend's 7,000 sats and 1,500 sats routed, at $100k
		t.Errorf("income %d", income)
	}
	for _, u := range rep.Unmatched {
		t.Errorf("left for review: %+v", u)
	}
	for _, a := range rep.Accounts {
		if a.Name == "Lightning node" && (a.ChainSats == nil || *a.ChainSats != a.Sats) {
			t.Errorf("node traced %d, reports %v", a.Sats, a.ChainSats)
		}
	}
}
