package app

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"btctax/internal/importers"
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
		"income-2025.csv", "gifts-donations-2025.csv", "transfers-2025.csv", "sources.csv", "proof/ex_4/1-food_bank_letter.pdf"} {
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
	for _, want := range []string{"tax year 2025", "Short-term", "FORM 1040 DIGITAL ASSET QUESTION\n  Yes", "per wallet and account", "OPEN ITEMS", "No 1099-DA imported", "lots-sold-2025.csv"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q:\n%s", want, readme)
		}
	}
	if files["proof/ex_4/1-food_bank_letter.pdf"] != "%PDF-1.4 synthetic" {
		t.Error("attached proof changed")
	}
}
