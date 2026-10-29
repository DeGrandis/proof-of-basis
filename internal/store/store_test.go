package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/importers"
)

// The kept file comes back exactly, even when it isn't UTF-8, and a
// changed copy is refused rather than handed back.
func TestImportFileRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acct, _ := st.ExchangeAccount("Kraken", "river")
	body := []byte("Date,Note\r\n2024-01-01,caf\xe9 \xff\x00\r\n") // Windows-1252, a stray byte, a NUL
	sum := sha256.Sum256(body)
	id, _, err := st.AddImport(acct, "river", "k.csv", hex.EncodeToString(sum[:]), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ImportFile(id); err != ErrNoImportFile {
		t.Errorf("before saving: %v", err)
	}
	if err := st.SaveImportFile(id, body); err != nil {
		t.Fatal(err)
	}
	name, got, err := st.ImportFile(id)
	if err != nil || name != "k.csv" || string(got) != string(body) {
		t.Fatalf("%q %q %v", name, got, err)
	}
	st.SaveImportFile(id, append(body, 'x'))
	if _, _, err := st.ImportFile(id); err == nil {
		t.Error("a file that doesn't match its hash was returned")
	}
}

func TestOverlappingImportsDoNotDuplicate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acct, _ := st.ExchangeAccount("River", "river")
	ev := func(uid string) importers.Event {
		return importers.Event{Kind: importers.Buy, Time: time.Unix(1, 0), Sats: 1, UID: uid}
	}
	if _, skipped, err := st.AddImport(acct, "river", "jan.csv", "a", []importers.Event{ev("r:1"), ev("r:2")}, nil); err != nil || skipped != 0 {
		t.Fatal(skipped, err)
	}
	_, skipped, err := st.AddImport(acct, "river", "jan-feb.csv", "b", []importers.Event{ev("r:1"), ev("r:2"), ev("r:3")}, nil)
	if err != nil || skipped != 2 {
		t.Fatal(skipped, err)
	}
	if _, _, err := st.AddImport(acct, "river", "jan-feb.csv", "b", nil, nil); err != ErrDuplicate {
		t.Fatal("same file accepted twice")
	}
	all, _ := st.ExchangeEvents()
	if len(all) != 3 {
		t.Errorf("%d events", len(all))
	}
}

func TestWipe(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetSetting("fulcrumHost", "node.local")
	st.AddWallet("cold", "xpub-placeholder", "p2wpkh", 20)
	acct, _ := st.ExchangeAccount("River", "river")
	st.AddImport(acct, "river", "a.csv", "a", []importers.Event{{Kind: importers.Buy, Time: time.Unix(1, 0), Sats: 1, UID: "r:1"}}, nil)
	st.PutPrices(map[string]int64{"2024-01-01": 1}, "import")
	if err := st.Wipe(); err != nil {
		t.Fatal(err)
	}
	accts, _ := st.Accounts()
	evs, _ := st.ExchangeEvents()
	ps, _ := st.Prices()
	if len(accts)+len(evs)+len(ps) != 0 || st.Setting("fulcrumHost", "") != "" {
		t.Errorf("left behind: %d accounts, %d events, %d prices, host %q", len(accts), len(evs), len(ps), st.Setting("fulcrumHost", ""))
	}
}

// Events stored by an older parser are read again from their saved rows:
// same ids and UIDs, new reading. Synthetic rows.
func TestReparseKeepsIDs(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acct, _ := st.ExchangeAccount("Coinbase", "coinbase")
	row := func(id, typ, qty, fee, notes string) map[string]string {
		return map[string]string{"ID": id, "Timestamp": "2023-02-01 10:00:00 UTC", "Transaction Type": typ, "Asset": "BTC",
			"Quantity Transacted": qty, "Price Currency": "USD", "Price at Transaction": "$20000.00", "Subtotal": "$100.00",
			"Total (inclusive of fees and/or spread)": "$100.00", "Fees and/or Spread": fee, "Notes": notes}
	}
	at := time.Date(2023, 2, 1, 10, 0, 0, 0, time.UTC)
	// As the old parser read them: the conversion into BTC as a sale, the
	// send without its USD fee.
	old := []importers.Event{
		{Kind: importers.Sell, Time: at, Sats: 500_000, FiatCents: 100_00, Currency: "USD", UID: "coinbase:a", Ref: "a", Row: 5,
			Raw: row("a", "Convert", "0.00500000", "$0.00", "Converted 100 USDC to 0.005 BTC")},
		{Kind: importers.Withdrawal, Time: at, Sats: 100_000, Currency: "USD", UID: "coinbase:b", Ref: "b", Row: 6,
			Raw: row("b", "Send", "-0.00100000", "$2.00", "")},
	}
	if _, _, err := st.AddImport(acct, "coinbase", "old.csv", "x", old, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ExchangeEvents()
	if n, err := st.Reparse(); err != nil || n != 2 {
		t.Fatalf("reparse changed %d, err %v", n, err)
	}
	after, _ := st.ExchangeEvents()
	if len(after) != 2 || after[0].ID != before[0].ID || after[1].ID != before[1].ID {
		t.Fatalf("ids changed: %+v -> %+v", before, after)
	}
	if e := after[0].Event; e.Kind != importers.Buy || e.Sats != 500_000 || e.UID != "coinbase:a" || e.Row != 5 {
		t.Errorf("convert %+v", e)
	}
	if e := after[1].Event; e.FeeSats != 10_000 || !e.FeeApprox { // $2 at $20,000
		t.Errorf("send %+v", e)
	}
	if n, _ := st.Reparse(); n != 0 {
		t.Errorf("second reparse changed %d", n)
	}
}
