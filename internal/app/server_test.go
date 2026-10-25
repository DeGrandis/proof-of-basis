package app

import (
	"btctax/internal/demo"
	"btctax/internal/importers"
	"btctax/internal/scan"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"btctax/internal/store"
)

// BIP84 test vector key (public test data, not anyone's wallet).
const zpub = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"
const xpub44 = "xpub6BosfCnifzxcFwrSzQiqu2DBVTshkCXacvNsWGYJVVhhawA7d4R5WSWGFNbi8Aw6ZRc1brxMyWMzG3DSSSSoekkudhUd9yLb6qx39T9nMdj"

func newTest(t *testing.T) (*store.Store, http.Handler) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Most tests exercise the API, so they run as a logged-in browser.
	if err := st.SetPasswordHash("test-only"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSession(tokenHash("test-token"), time.Now()); err != nil {
		t.Fatal(err)
	}
	h := New(st, fstest.MapFS{}).Handler()
	return st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test-token"})
		h.ServeHTTP(w, r)
	})
}

func call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEmptyReportHasNoNullLists(t *testing.T) {
	_, h := newTest(t)
	w := call(h, "GET", "/api/report", "")
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"accounts", "years", "disposals", "removals", "income", "unmatched", "links", "graph", "warnings"} {
		if _, ok := m[k].([]any); !ok {
			t.Errorf("%s is %v, want a list", k, m[k])
		}
	}
}

func TestSameWalletRefused(t *testing.T) {
	_, h := newTest(t)
	if w := call(h, "POST", "/api/wallets", `{"name":"Cold","key":"`+zpub+`"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, body := range []string{
		`{"name":"Cold again","key":"` + zpub + `"}`,
		`{"name":"Cold again","key":"` + zpub + `","script":"p2wpkh"}`,
		`{"name":"cold","key":"` + xpub44 + `"}`, // name clash, any case
	} {
		if w := call(h, "POST", "/api/wallets", body); w.Code != 409 {
			t.Errorf("%s: got %d %s", body, w.Code, w.Body.String())
		}
	}
	// The same key with a different script type is a different wallet.
	if w := call(h, "POST", "/api/wallets", `{"name":"Legacy","key":"`+xpub44+`"}`); w.Code != 200 {
		t.Error(w.Body.String())
	}
	if w := call(h, "POST", "/api/wallets", `{"name":"Legacy segwit","key":"`+xpub44+`","script":"p2wpkh"}`); w.Code != 200 {
		t.Error(w.Body.String())
	}
}

func TestWipe(t *testing.T) {
	st, h := newTest(t)
	call(h, "POST", "/api/settings", `{"fulcrumHost":"node.local"}`)
	call(h, "POST", "/api/wallets", `{"name":"Cold","key":"`+zpub+`"}`)
	if w := call(h, "POST", "/api/wipe", `{}`); w.Code != 400 {
		t.Fatalf("wipe without confirmation: %d", w.Code)
	}
	if w := call(h, "POST", "/api/wipe", `{"confirm":"WIPE"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if a, _ := st.Accounts(); len(a) != 0 || st.Setting("fulcrumHost", "") != "" {
		t.Errorf("left behind: %v %q", a, st.Setting("fulcrumHost", ""))
	}
}

func upload(h http.Handler, name, content, handler string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	mw.WriteField("handler", handler)
	mw.Close()
	r := httptest.NewRequest("POST", "/api/imports", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The drop zone relies on these codes: 422 means "ask for the platform",
// 409 means "already imported".
func TestImportStatusCodes(t *testing.T) {
	_, h := newTest(t)
	river, err := os.ReadFile("../importers/testdata/river.csv")
	if err != nil {
		t.Fatal(err)
	}
	if w := upload(h, "river.csv", string(river), ""); w.Code != 200 {
		t.Fatalf("detect: %d %s", w.Code, w.Body.String())
	}
	if w := upload(h, "river.csv", string(river), ""); w.Code != 409 {
		t.Errorf("same file again: %d", w.Code)
	}
	if w := upload(h, "mystery.csv", "when,what,how much\n2024-01-01,thing,1\n", ""); w.Code != 422 {
		t.Errorf("unknown format: %d %s", w.Code, w.Body.String())
	}
}

func TestFulcrumStatusNotSetUp(t *testing.T) {
	_, h := newTest(t)
	var st struct {
		OK    bool
		Error string
	}
	json.Unmarshal(call(h, "GET", "/api/fulcrum/status", "").Body.Bytes(), &st)
	if st.OK || !strings.Contains(st.Error, "address first") {
		t.Errorf("%+v", st)
	}
}

// The demo history is large and exercises every path: it must load, trace
// with no coins of unknown origin, and give the flow view plenty to draw.
func TestDemoReport(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	if err := demo.Seed(st, 1); err == nil {
		t.Fatal("seeded a database that already had data")
	}
	var rep Report
	w := call(h, "GET", "/api/report", "")
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(w.Code, err)
	}
	kinds := map[string]int{}
	for _, g := range rep.Graph {
		kinds[g.Kind]++
	}
	t.Logf("graph %d events %v, %d disposals, %d unmatched, %d warnings", len(rep.Graph), kinds, len(rep.Disposals), len(rep.Unmatched), len(rep.Warnings))
	if len(rep.Graph) < 500 || kinds["transfer"] < 50 || kinds["consolidation"] == 0 || len(rep.Unmatched) == 0 {
		t.Errorf("too small: %d events %v, %d unmatched", len(rep.Graph), kinds, len(rep.Unmatched))
	}
	for _, msg := range rep.Warnings {
		if strings.Contains(msg, "no known basis") || strings.Contains(msg, "coinjoin") {
			t.Errorf("warning: %s", msg)
		}
	}
	// Every wallet's traced balance matches the chain once the unlabeled
	// items still waiting in Review are accounted for.
	pending := map[string]int64{}
	for _, it := range rep.Unmatched {
		if it.Dir == "out" {
			pending[it.Account] += it.Sats + it.FeeSats
		} else {
			pending[it.Account] -= it.Sats
		}
	}
	for _, a := range rep.Accounts {
		if a.ChainSats != nil && *a.ChainSats+pending[a.Name] != a.Sats {
			t.Errorf("%s: traced %d, on chain %d, pending review %d", a.Name, a.Sats, *a.ChainSats, pending[a.Name])
		}
	}
}

func TestHistoryAndFreshness(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	if len(rep.History) < 2000 {
		t.Fatalf("%d history points", len(rep.History))
	}
	if last := rep.History[len(rep.History)-1]; last.Sats != rep.Holdings {
		t.Errorf("history ends at %d sats, holdings %d", last.Sats, rep.Holdings)
	}
	var fresh []struct {
		Account string
		Last    string
		Days    int
	}
	json.Unmarshal(call(h, "GET", "/api/imports/freshness", "").Body.Bytes(), &fresh)
	if len(fresh) != 3 {
		t.Errorf("%+v", fresh)
	}
}

func TestWhatIfAndProtect(t *testing.T) {
	st, h := newTest(t)
	acct, _ := st.ExchangeAccount("River", "river")
	buy := func(uid, when string, sats, cents int64) importers.Event {
		tm, _ := time.Parse("2006-01-02", when)
		return importers.Event{Kind: importers.Buy, Time: tm, Sats: sats, FiatCents: cents, UID: uid}
	}
	st.AddImport(acct, "river", "a.csv", "a", []importers.Event{
		buy("r:1", "2020-01-01", 100_000_000, 7_000_00),  // long term, cheap
		buy("r:2", "2026-01-01", 100_000_000, 90_000_00), // short term, dear
	}, nil)
	st.PutPrices(map[string]int64{time.Now().UTC().Format("2006-01-02"): 100_000_00}, "import")
	ask := func() map[string]any {
		var out map[string]any
		w := call(h, "POST", "/api/whatif", `{"account":"River","btc":0.5}`)
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 {
			t.Fatalf("%d %v", w.Code, out)
		}
		return out
	}
	r := ask() // FIFO: the 2020 coins, long term
	if r["long"].(map[string]any)["sats"].(float64) != 50_000_000 || r["proceeds"].(float64) != 50_000_00 {
		t.Errorf("FIFO %v", r)
	}
	call(h, "POST", "/api/protect", `{"key":"ex:1","on":true}`)
	r = ask() // the 2020 lot is protected, so the 2026 coins go first
	if r["short"].(map[string]any)["sats"].(float64) != 50_000_000 {
		t.Errorf("protected %v", r)
	}
	if w := call(h, "POST", "/api/whatif", `{"account":"River","btc":5}`); w.Code != 400 {
		t.Errorf("more than held: %d", w.Code)
	}
}

func TestAddMultisigWallet(t *testing.T) {
	_, h := newTest(t)
	body := `{"name":"Vault","multisig":{"threshold":2,"sorted":true,"keys":["` + zpub + `","` + xpub44 + `"]}}`
	if w := call(h, "POST", "/api/wallets/preview", body); w.Code != 200 || !strings.Contains(w.Body.String(), "wsh(sortedmulti(2,") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := call(h, "POST", "/api/wallets", body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

// Lookups answer from local records without Fulcrum; recheck with no
// wallets finishes at once; paths with txids or addresses aren't logged.
func TestLookupsAndRecheck(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	var txid, addr string
	for _, g := range rep.Graph {
		if g.Kind == "transfer" && strings.Contains(g.Key, ":from:") {
			txid = g.Txid
		}
		if g.Kind == "unmatched" && g.Address != "" && addr == "" {
			addr = g.Address
		}
	}
	var tv txView
	w := call(h, "GET", "/api/tx/"+txid, "")
	json.Unmarshal(w.Body.Bytes(), &tv)
	if w.Code != 200 || tv.Source != "wallets" || len(tv.Inputs) < 2 || tv.FeeSats <= 0 {
		t.Errorf("%d %+v", w.Code, tv)
	}
	wallets := map[string]bool{}
	for _, in := range tv.Inputs {
		wallets[in.Wallet] = true
	}
	if len(wallets) < 2 {
		t.Errorf("inputs from %v", wallets)
	}
	if w := call(h, "GET", "/api/tx/nothex", ""); w.Code != 400 {
		t.Errorf("bad txid: %d", w.Code)
	}
	var av struct {
		Txs []addrTx
	}
	json.Unmarshal(call(h, "GET", "/api/address/"+addr, "").Body.Bytes(), &av)
	if len(av.Txs) != 1 || !av.Txs[0].Local {
		t.Errorf("address %s: %+v", addr, av)
	}

	st2, h2 := newTest(t)
	_ = st2
	call(h2, "POST", "/api/recheck", "")
	for i := 0; i < 50; i++ {
		var rs recheckState
		json.Unmarshal(call(h2, "GET", "/api/recheck", "").Body.Bytes(), &rs)
		if !rs.Running && rs.Finished != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("recheck never finished")
}

// Picking specific lots sells exactly those, whatever the year's method.
func TestPickedLots(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	// Pick the two newest lots in one account and sell exactly them.
	var acct string
	var ids []string
	var want int64
	for i := len(rep.OpenLots) - 1; i >= 0 && len(ids) < 2; i-- {
		l := rep.OpenLots[i]
		if acct == "" {
			acct = l.Account
		}
		if l.Account == acct {
			ids = append(ids, l.ID)
			want += l.Sats
		}
	}
	b, _ := json.Marshal(map[string]any{"account": acct, "lots": ids})
	var wi map[string]any
	w := call(h, "POST", "/api/whatif", string(b))
	json.Unmarshal(w.Body.Bytes(), &wi)
	if w.Code != 200 || int64(wi["sats"].(float64)) != want {
		t.Errorf("picked %d sats in %s: %d %v", want, acct, w.Code, wi)
	}
}

// Ways an exchange balance can drift from what the exchange shows, on
// invented data. The traced balance only moves when a row is matched or
// labeled, so unlabeled sends still count as held; the report says so.
func TestExchangeBalanceDrift(t *testing.T) {
	st, h := newTest(t)
	acct, _ := st.ExchangeAccount("Strike", "strike")
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.AddImport(acct, "strike", "s.csv", "s", []importers.Event{
		{Kind: importers.Buy, Time: at("2025-01-01"), Sats: 1_000_000, FiatCents: 500_00, UID: "s1"},
		// A Lightning payment with its routing fee, not labeled yet.
		{Kind: importers.Withdrawal, Time: at("2025-02-01"), Sats: 100_000, FeeSats: 10, Address: "lnbc1synthetic", UID: "s2"},
		// An on-chain send to an address that isn't in any added wallet.
		{Kind: importers.Withdrawal, Time: at("2025-03-01"), Sats: 200_000, FeeSats: 500, Address: "bc1qsynthetic", UID: "s3"},
		// A sale: leaves right away.
		{Kind: importers.Sell, Time: at("2025-04-01"), Sats: 50_000, FiatCents: 40_00, UID: "s4"},
	}, nil)
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	var a AccountView
	for _, x := range rep.Accounts {
		if x.Name == "Strike" {
			a = x
		}
	}
	if a.Sats != 950_000 {
		t.Errorf("traced balance %d: unlabeled sends should still be counted", a.Sats)
	}
	if a.PendingOut != 300_510 {
		t.Errorf("pending out %d, want the two unlabeled sends plus fees", a.PendingOut)
	}
	if a.Sats-a.PendingOut != 649_490 { // what Strike itself shows
		t.Errorf("after labels %d", a.Sats-a.PendingOut)
	}
	// Labeling the sends makes the balance match the exchange.
	call(h, "POST", "/api/labels", `{"Key":"ex:2","Category":"spend"}`)
	call(h, "POST", "/api/labels", `{"Key":"ex:3","Category":"self-transfer"}`)
	var rep2 Report // a fresh value: omitted zero fields would otherwise keep old values
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep2)
	for _, x := range rep2.Accounts {
		if x.Name == "Strike" && (x.Sats != 649_490 || x.PendingOut != 0) {
			t.Errorf("after labeling: %d pending %d", x.Sats, x.PendingOut)
		}
	}
}

// A sale bigger than anything bought: the report names the first record
// that took the account below zero, by a key the Flow page can open.
// Invented data.
func TestReportNamesFirstDip(t *testing.T) {
	st, h := newTest(t)
	acct, _ := st.ExchangeAccount("Coinbase", "coinbase")
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.AddImport(acct, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2021-01-01"), Sats: 100_000, FiatCents: 300_00, UID: "c1"},
		{Kind: importers.Sell, Time: at("2021-02-01"), Sats: 150_000, FiatCents: 500_00, UID: "c2"}, // the 50,000 extra came from nowhere
		{Kind: importers.Sell, Time: at("2021-03-01"), Sats: 10_000, FiatCents: 40_00, UID: "c3"},
	}, nil)
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	if len(rep.Dips) != 1 {
		t.Fatalf("dips %+v", rep.Dips)
	}
	d := rep.Dips[0]
	if d.Account != "Coinbase" || d.Balance != -50_000 || d.T != at("2021-02-01").Unix() {
		t.Errorf("dip %+v", d)
	}
	found := false
	for _, g := range rep.Graph {
		found = found || g.Key == d.Key
	}
	if !found {
		t.Errorf("dip key %q is not in the graph", d.Key)
	}
}

// A wallet send whose inputs are all the wallet's own, but one coin it spent
// arrived in a receipt still waiting in Review: the dip says what the
// wallet really held on chain and which incoming record is uncounted.
// Invented data.
func TestDipExplainsUncountedReceipt(t *testing.T) {
	st, h := newTest(t)
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	og, _ := st.AddWallet("OG", "xpub-placeholder", "p2pkh", 20)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2017-06-01"), Sats: 6_962_891, FiatCents: 200_00, UID: "c1"},
		{Kind: importers.Withdrawal, Time: at("2017-06-02"), Sats: 6_962_891, UID: "c2"},
	}, nil)
	in := func(txid string, sats int64, day string) scan.Tx {
		return scan.Tx{Txid: txid, Height: 100, Time: at(day), Inputs: []scan.Input{{PrevTxid: "elsewhere-" + txid}},
			Outputs: []scan.Output{{Vout: 0, Address: "og-" + txid, Sats: sats, Ours: true}}, RecvSats: sats}
	}
	spend := scan.Tx{Txid: "cc", Height: 300, Time: at("2017-12-31"),
		Inputs:   []scan.Input{{PrevTxid: "aa", Sats: 6_962_891, Ours: true}, {PrevTxid: "bb", Sats: 5_700_000, Ours: true}},
		Outputs:  []scan.Output{{Vout: 0, Address: "x1", Sats: 10_000}, {Vout: 1, Address: "gone", Sats: 12_625_585}},
		SentSats: 12_662_891, FeeSats: 27_306}
	st.SaveScan(og, &scan.Result{Txs: []scan.Tx{in("aa", 6_962_891, "2017-06-02"), in("bb", 5_700_000, "2017-09-01"), spend}})
	call(h, "POST", "/api/labels", `{"Key":"tx:cc:0","Category":"spend"}`)
	call(h, "POST", "/api/labels", `{"Key":"tx:cc:1","Category":"lost"}`)
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	if len(rep.Dips) != 1 {
		t.Fatalf("dips %+v", rep.Dips)
	}
	d := rep.Dips[0]
	if d.Account != "OG" || d.Key != "tx:cc:1" || d.Balance != -5_700_000 || d.Before != 6_925_585 {
		t.Errorf("dip %+v", d)
	}
	if d.Chain == nil || *d.Chain != 12_662_891 {
		t.Errorf("chain before %v", d.Chain)
	}
	if len(d.Uncounted) != 1 || d.Uncounted[0] != "tx:bb:0" || d.UncountedSats != 5_700_000 {
		t.Errorf("uncounted %v %d", d.Uncounted, d.UncountedSats)
	}
}
