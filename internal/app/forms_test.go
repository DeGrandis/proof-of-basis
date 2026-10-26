package app

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/importers"
)

// A year's Form 8949 lines against a broker's 1099-DA. Invented data.
func TestForm8949AgainstThe1099DA(t *testing.T) {
	st, h := newTest(t)
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	rv, _ := st.ExchangeAccount("River", "river")
	st.AddImport(cb, "coinbase", "c.csv", "c", []importers.Event{
		{Kind: importers.Buy, Time: at("2023-01-10"), Sats: 1_000_000, FiatCents: 200_00, UID: "b1"},
		{Kind: importers.Buy, Time: at("2025-02-01"), Sats: 3_500_000, FiatCents: 2_625_00, UID: "b2"},
		// Long-term (bought 2023), on the 1099-DA without basis.
		{Kind: importers.Sell, Time: at("2025-03-01"), Sats: 1_000_000, FiatCents: 900_00, UID: "s1"},
		// Short-term, $10 fee; the 1099-DA reports a wrong basis.
		{Kind: importers.Sell, Time: at("2025-04-01"), Sats: 2_000_000, FiatCents: 2_000_00, FeeCents: 10_00, UID: "s2"},
		// Short-term, missing from the 1099-DA.
		{Kind: importers.Sell, Time: at("2025-05-01"), Sats: 500_000, FiatCents: 450_00, UID: "s3"},
	}, nil)
	st.AddImport(rv, "river", "r.csv", "r", []importers.Event{
		// Bought and sold out before 2025, so the January 2025 split of
		// the pool doesn't mix River's coins into Coinbase.
		{Kind: importers.Buy, Time: at("2022-01-01"), Sats: 200_000, FiatCents: 80_00, UID: "rb"},
		{Kind: importers.Sell, Time: at("2024-06-01"), Sats: 200_000, FiatCents: 120_00, UID: "r24"},
		{Kind: importers.Buy, Time: at("2025-06-01"), Sats: 300_000, FiatCents: 280_00, UID: "rb2"},
		{Kind: importers.Sell, Time: at("2025-07-01"), Sats: 300_000, FiatCents: 300_00, UID: "r25"},
	}, nil)

	// The broker's 1099-DA, by box number, as a CSV upload.
	csvText := "1b,1c,1d,1e,1f,1g,2,6,9\n" +
		"Bitcoin,0.01,01/10/2023,03/01/2025,900.00,,,Long-term,X\n" +
		"Bitcoin,0.02,02/01/2025,04/01/2025,1990.00,1400.00,X,Short-term,\n" +
		"Bitcoin,0.003,,06/01/2025,280.00,,,Short-term,X\n" +
		"Ethereum,1.5,,06/02/2025,4000.00,,,Short-term,X\n"
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("account", "Coinbase")
	fw, _ := mw.CreateFormFile("file", "coinbase-1099da-2025.csv")
	fw.Write([]byte(csvText))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/1099da", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"rows":3`) {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}

	var f Forms
	json.Unmarshal(call(h, "GET", "/api/forms?year=2025", "").Body.Bytes(), &f)
	by := map[string]F8949Row{}
	for _, r := range f.Rows {
		by[r.Account+" "+r.Sold] = r
	}
	if r := by["Coinbase 03/01/2025"]; r.Box != "K" || r.Status != statusMatched || r.Adjustment != 0 || r.Gain != 700_00 || r.Acquired != "01/10/2023" {
		t.Errorf("long-term, basis not reported: %+v", r)
	}
	// Reported basis $1,400 against the true $1,500: code B, (g) = -100,
	// so (d) - (e) + (g) = 1,990 - 1,400 - 100 = the true $490 gain.
	if r := by["Coinbase 04/01/2025"]; r.Box != "G" || r.Code != "B" || r.Basis != 1_400_00 || r.Adjustment != -100_00 || r.Gain != 490_00 || r.Proceeds != 1_990_00 {
		t.Errorf("wrong reported basis: %+v", r)
	}
	if r := by["Coinbase 05/01/2025"]; r.Box != "I" || r.Status != statusNotOnDA {
		t.Errorf("missing from the 1099-DA: %+v", r)
	}
	if r := by["River 07/01/2025"]; r.Box != "H" || r.Status != statusNoDA {
		t.Errorf("exchange without a 1099-DA yet: %+v", r)
	}
	if len(f.DAOnly) != 1 || f.DAOnly[0].Row.Sats != 300_000 {
		t.Errorf("1099-DA lines with no sale: %+v", f.DAOnly)
	}
	if f.Boxes["G"] != 1 || f.Boxes["K"] != 1 || f.Boxes["I"] != 1 || f.Boxes["H"] != 1 {
		t.Errorf("boxes %v", f.Boxes)
	}

	// Before 2025: boxes C and F.
	var f24 Forms
	json.Unmarshal(call(h, "GET", "/api/forms?year=2024", "").Body.Bytes(), &f24)
	if len(f24.Rows) != 1 || f24.Rows[0].Box != "F" || f24.Rows[0].Status != statusPre2025 {
		t.Errorf("2024: %+v", f24.Rows)
	}

	// The CSVs: one box, and the reconciliation.
	out := call(h, "GET", "/api/export/8949.csv?year=2025&box=g", "").Body.String()
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "G,0.02000000 BTC") {
		t.Errorf("box G csv:\n%s", out)
	}
	rec := call(h, "GET", "/api/export/1099da-reconciliation.csv?year=2025", "").Body.String()
	if !strings.Contains(rec, "only on the 1099-DA") || !strings.Contains(rec, statusNotOnDA) {
		t.Errorf("reconciliation:\n%s", rec)
	}
}
