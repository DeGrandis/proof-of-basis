package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"btctax/internal/importers"
	"btctax/internal/scan"
	"btctax/internal/store"
)

// The accountant package for a year: every file there, provenance traced
// through a transfer, attached proof included, and a README that states
// the numbers, the assumptions and what is still open. Invented data.
func TestAccountantPackage(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.PutPrices(map[string]int64{"2025-05-01": 90_000_00, "2025-06-01": 95_000_00}, "test")
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	rv, _ := st.ExchangeAccount("River", "river")
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2025-01-10"), Sats: 2_000_000, FiatCents: 1_000_00, UID: "b"},
		{Kind: importers.Withdrawal, Time: at("2025-02-01"), Sats: 1_000_000, UID: "w"},
		{Kind: importers.Income, Time: at("2025-05-01"), Sats: 10_000, FiatCents: 9_00, Note: "Staking reward", UID: "i"},
		{Kind: importers.Withdrawal, Time: at("2025-06-01"), Sats: 100_000, Address: "bc1qcharity", UID: "d"},
	}, nil)
	st.AddImport(rv, "river", "r.csv", "r", []importers.Event{
		{Kind: importers.Deposit, Time: at("2025-02-01"), Sats: 1_000_000, UID: "dep"},
		{Kind: importers.Sell, Time: at("2025-04-01"), Sats: 1_000_000, FiatCents: 800_00, UID: "s"},
	}, nil)
	call(h, "POST", "/api/labels", `{"Key":"ex:4","Category":"donation","Recipient":"Food Bank"}`)
	// The charity's letter, attached to the donation.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("key", "ex:4")
	fw, _ := mw.CreateFormFile("file", "food bank letter.pdf")
	fw.Write([]byte("%PDF-1.4 synthetic"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(httptest.NewRecorder(), req)

	w := call(h, "GET", "/api/package/2025.zip", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("package: %d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[strings.TrimPrefix(f.Name, "btc-tax-2025/")] = string(b)
	}
	for _, want := range []string{"README.txt", "form8949-2025.csv", "form8949-2025-box-H.csv", "1099da-reconciliation-2025.csv", "lots-sold-2025.csv",
		"income-2025.csv", "gifts-donations-2025.csv", "transfers-2025.csv", "sources.csv"} {
		if _, ok := files[want]; !ok {
			names := []string{}
			for n := range files {
				names = append(names, n)
			}
			t.Errorf("missing %s; have %v", want, names)
		}
	}
	// The River sale traces back to the Coinbase purchase through the transfer.
	if l := files["lots-sold-2025.csv"]; !strings.Contains(l, "2025-01-10") || !strings.Contains(l, "Coinbase to River") {
		t.Errorf("provenance:\n%s", l)
	}
	if i := files["income-2025.csv"]; !strings.Contains(i, "staking") || !strings.Contains(i, "from the export") {
		t.Errorf("income:\n%s", i)
	}
	readme := files["README.txt"]
	for _, want := range []string{"tax year 2025", "Short-term", "FORM 1040 DIGITAL ASSET QUESTION\n  Yes", "per wallet and account", "OPEN ITEMS", "No 1099-DA from River", "lots-sold-2025.csv"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q:\n%s", want, readme)
		}
	}
	// Proof sits under the record's reference, not its key.
	proof := ""
	for n, b := range files {
		if strings.HasPrefix(n, "proof/R-") && strings.HasSuffix(n, "/1-food_bank_letter.pdf") {
			proof = b
		}
	}
	if proof != "%PDF-1.4 synthetic" {
		t.Error("attached proof missing or changed")
	}
}

// Exports carry no on-chain data unless asked: no txids, no addresses, no
// record keys that embed a txid, in the package or the CSVs. Invented
// wallet history.
func TestExportsHaveNoOnchainData(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	st.PutPrices(map[string]int64{"2025-03-01": 90_000_00, "2025-04-01": 90_000_00}, "test")
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	const recvTx = "aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11"
	const spendTx = "bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22"
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2025-01-10"), Sats: 1_000_000, FiatCents: 900_00, UID: "b", Ref: "cb-buy-1"},
		// Coinbase's record carries the txid and the receiving address.
		{Kind: importers.Withdrawal, Time: at("2025-03-01"), Sats: 1_000_000, Txid: recvTx, Address: "bc1qreceivingaddressxyz", UID: "w"},
	}, nil)
	og, _ := st.AddWallet("Cold", "xpub-placeholder", "p2wpkh", 20)
	st.SaveScan(og, &scan.Result{Txs: []scan.Tx{
		{Txid: recvTx, Height: 100, Time: at("2025-03-01"), Inputs: []scan.Input{{PrevTxid: "cc33cc33cc33cc33cc33cc33cc33cc33cc33cc33cc33cc33cc33cc33cc33cc33"}},
			Outputs: []scan.Output{{Vout: 0, Address: "bc1qreceivingaddressxyz", Sats: 1_000_000, Ours: true}}, RecvSats: 1_000_000},
		{Txid: spendTx, Height: 200, Time: at("2025-04-01"), Inputs: []scan.Input{{PrevTxid: recvTx, Sats: 1_000_000, Ours: true}},
			Outputs: []scan.Output{{Vout: 0, Address: "bc1qshopaddressxyz", Sats: 990_000}}, SentSats: 1_000_000, FeeSats: 10_000},
	}})
	call(h, "POST", "/api/labels", `{"Key":"tx:`+spendTx+`:0","Category":"spend","FiatCents":89000}`)
	// Proof attached to the spend.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("key", "tx:"+spendTx+":0")
	fw, _ := mw.CreateFormFile("file", "receipt.txt")
	fw.Write([]byte("shop receipt"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(httptest.NewRecorder(), req)

	leaks := func(name, text string) {
		t.Helper()
		for _, bad := range []string{recvTx, spendTx, "tx:", "bc1q"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
		if hex64.MatchString(text) {
			t.Errorf("%s contains a 64-hex id", name)
		}
	}
	unzip := func(b []byte) map[string]string {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			c, _ := io.ReadAll(rc)
			rc.Close()
			out[f.Name] = string(c)
		}
		return out
	}
	pkg := unzip(call(h, "GET", "/api/package/2025.zip", "").Body.Bytes())
	if len(pkg) < 5 {
		t.Fatalf("package has %d files", len(pkg))
	}
	for name, text := range pkg {
		leaks(name, name)
		leaks(name, text)
	}
	leaks("8949 csv", call(h, "GET", "/api/export/8949.csv?year=2025", "").Body.String())
	leaks("reconciliation csv", call(h, "GET", "/api/export/1099da-reconciliation.csv?year=2025", "").Body.String())
	// The exchange's own id stays; the spend is findable by its reference.
	if !strings.Contains(pkg["btc-tax-2025/lots-sold-2025.csv"], "cb-buy-1") {
		t.Errorf("platform id dropped:\n%s", pkg["btc-tax-2025/lots-sold-2025.csv"])
	}
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	ref := ""
	for _, g := range rep.Graph {
		if g.Key == "tx:"+spendTx+":0" {
			ref = g.Ref
		}
	}
	if ref == "" || !strings.Contains(pkg["btc-tax-2025/form8949-2025.csv"], ref) {
		t.Errorf("reference %q not in the 8949 file", ref)
	}
	// Asked for explicitly, on-chain ids are included.
	all := unzip(call(h, "GET", "/api/package/2025.zip?onchain=1", "").Body.Bytes())
	if !strings.Contains(all["btc-tax-2025/form8949-2025.csv"], spendTx) {
		t.Error("onchain=1 left the txid out")
	}
}

// A 1099-DA PDF, checked by the totals typed from it, bundled into the
// package with a README that says how it was checked. Invented data.
func TestTaxFormPDF(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	rv, _ := st.ExchangeAccount("River", "river")
	st.AddImport(rv, "river", "r.csv", "r", []importers.Event{
		{Kind: importers.Buy, Time: at("2025-01-05"), Sats: 1_000_000, FiatCents: 900_00, UID: "b"},
		{Kind: importers.Sell, Time: at("2025-03-01"), Sats: 400_000, FiatCents: 400_00, FeeCents: 2_00, UID: "s1"},
		{Kind: importers.Sell, Time: at("2025-06-01"), Sats: 300_000, FiatCents: 330_00, UID: "s2"},
	}, nil)
	// River's proceeds: 398.00 + 330.00 = 728.00, net of the fee.
	upload := func(proceeds string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("account", "River")
		mw.WriteField("year", "2025")
		mw.WriteField("proceeds", proceeds)
		fw, _ := mw.CreateFormFile("file", "River 1099-DA 2025.pdf")
		fw.Write([]byte("%PDF-1.7 synthetic 1099-DA"))
		mw.Close()
		req := httptest.NewRequest("POST", "/api/taxforms", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("upload: %d %s", w.Code, w.Body.String())
		}
	}
	check := func() (Readiness, string) {
		var r Readiness
		json.Unmarshal(call(h, "GET", "/api/readiness?year=2025", "").Body.Bytes(), &r)
		for _, c := range r.Checks {
			if c.ID == "1099da:River" {
				return r, c.Status + " " + c.Title
			}
		}
		return r, ""
	}
	if _, c := check(); !strings.HasPrefix(c, "warn No 1099-DA from River") {
		t.Errorf("before any form: %q", c)
	}
	upload("$1,000.00")
	if r, c := check(); !strings.HasPrefix(c, "bad River's 1099-DA: $1,000.00; your records: $728.00") || r.Ready {
		t.Errorf("wrong total: %q (ready %v)", c, r.Ready)
	}
	var forms []store.TaxForm
	json.Unmarshal(call(h, "GET", "/api/taxforms?year=2025", "").Body.Bytes(), &forms)
	if len(forms) != 1 {
		t.Fatalf("forms %+v", forms)
	}
	call(h, "POST", "/api/taxforms/"+strconv.FormatInt(forms[0].ID, 10), `{"Proceeds":"728.00"}`)
	if _, c := check(); !strings.HasPrefix(c, "ok River's 1099-DA matches") {
		t.Errorf("right total: %q", c)
	}
	w := call(h, "GET", "/api/package/2025.zip", "")
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[strings.TrimPrefix(f.Name, "btc-tax-2025/")] = string(b)
	}
	if files["1099-DA/River/River_1099-DA_2025.pdf"] != "%PDF-1.7 synthetic 1099-DA" {
		names := []string{}
		for n := range files {
			names = append(names, n)
		}
		t.Errorf("form not bundled: %v", names)
	}
	readme := files["README.txt"]
	for _, want := range []string{"FORMS 1099-DA", "Form: 1099-DA/River/River_1099-DA_2025.pdf", "These records: 2 sales, proceeds $728.00",
		"match these records. Checked by totals, not line by line", "confirm box 2"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q:\n%s", want, readme)
		}
	}
}
