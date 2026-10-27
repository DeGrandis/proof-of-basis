package importers

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Fixtures in testdata are synthetic: invented amounts, ids and dates. The
// only address used is the BIP173 example address.

func load(t *testing.T, file, want string) []Event {
	t.Helper()
	f, err := os.Open("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h, ev, warns, err := Import(f, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.ID() != want {
		t.Fatalf("detected %s, want %s", h.ID(), want)
	}
	for _, w := range warns {
		t.Logf("%s line %d: %s", file, w.Row, w.Message)
	}
	return ev
}

func TestCoinbase(t *testing.T) {
	ev := load(t, "coinbase.csv", "coinbase")
	if len(ev) != 6 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	if ev[0].Kind != Buy || ev[0].Sats != 1_000_000 || ev[0].FiatCents != 304_99 || ev[0].FeeCents != 4_99 {
		t.Errorf("buy %+v", ev[0])
	}
	if ev[1].Kind != Withdrawal || ev[1].Address != "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq" || ev[1].Sats != 500_000 {
		t.Errorf("send %+v", ev[1])
	}
	if ev[2].Kind != Sell || ev[2].FiatCents != 100_00 || ev[2].FeeCents != 1_49 {
		t.Errorf("sell %+v", ev[2])
	}
	if ev[3].Kind != Income || ev[3].FiatCents != 19 {
		t.Errorf("earn %+v", ev[3])
	}
	if ev[4].Kind != Sell || ev[4].Sats != 10_000 || ev[4].FiatCents != 2_30 {
		t.Errorf("card spend %+v", ev[4])
	}
	if ev[5].Kind != Buy || ev[5].Sats != 4_000_000 || ev[5].FiatCents != 812_00 {
		t.Errorf("convert into BTC %+v", ev[5])
	}
	if ev[0].UID != "coinbase:syn001" {
		t.Errorf("uid %q", ev[0].UID)
	}
}

func TestRiver(t *testing.T) {
	ev := load(t, "river.csv", "river")
	if len(ev) != 4 {
		t.Fatalf("got %d events", len(ev))
	}
	want := []struct {
		k          Kind
		sats, fiat int64
	}{{Buy, 590_000, 100_00}, {Withdrawal, 500_000, 0}, {Income, 500, 0}, {Sell, 90_000, 55_50}}
	for i, w := range want {
		if ev[i].Kind != w.k || ev[i].Sats != w.sats || ev[i].FiatCents != w.fiat {
			t.Errorf("row %d: %+v", i, ev[i])
		}
	}
	if ev[1].FeeSats != 2_000 {
		t.Errorf("withdrawal fee %d", ev[1].FeeSats)
	}
}

// Strike's account report: different column names, same shape. Referral
// and Strike Credit are BTC income valued at the day's price.
func TestStrikeAccountReport(t *testing.T) {
	ev := load(t, "strike-report.csv", "strike")
	kinds := []Kind{FiatMove, Buy, Income, Income, Deposit, Withdrawal, Withdrawal, FiatMove}
	if len(ev) != len(kinds) {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for i, k := range kinds {
		if ev[i].Kind != k {
			t.Errorf("row %d: kind %s want %s", i, ev[i].Kind, k)
		}
	}
	if ev[1].Sats != 150_000 || ev[1].FiatCents != 100_00 || ev[1].FeeCents != 99 || ev[1].Ref != "syn-r-2" {
		t.Errorf("buy %+v", ev[1])
	}
	if ev[2].Sats != 5_000 || ev[2].FiatCents != 335 { // 0.00005 BTC at $67,000
		t.Errorf("referral %+v", ev[2])
	}
	if ev[5].FeeSats != 200 || ev[5].Address == "" {
		t.Errorf("send %+v", ev[5])
	}
}

func TestStrike(t *testing.T) {
	ev := load(t, "strike.csv", "strike")
	kinds := []Kind{FiatMove, Buy, Withdrawal, Sell, FiatMove}
	if len(ev) != len(kinds) {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for i, k := range kinds {
		if ev[i].Kind != k {
			t.Errorf("row %d: kind %s want %s", i, ev[i].Kind, k)
		}
	}
	if ev[1].Sats != 560_000 || ev[1].FiatCents != 250_00 || ev[1].Time.Format("2006-01-02 15:04:05") != "2024-01-05 14:00:01" {
		t.Errorf("buy %+v", ev[1])
	}
	if ev[2].FeeSats != 1_500 || ev[2].Address == "" || ev[2].Txid == "" {
		t.Errorf("send %+v", ev[2])
	}
	if ev[3].Sats != 200_000 || ev[3].FiatCents != 90_50 || ev[3].FeeCents != 50 {
		t.Errorf("sell %+v", ev[3])
	}
}

func TestFold(t *testing.T) {
	ev := load(t, "fold.csv", "fold")
	if len(ev) != 3 {
		t.Fatalf("got %d events", len(ev))
	}
	if ev[0].Kind != Buy || ev[0].FiatCents != 101_00 || ev[0].FeeCents != 1_00 || ev[0].Txid != "" || ev[0].UID != "fold:syn-f-1" {
		t.Errorf("buy %+v", ev[0])
	}
	if ev[0].Time.Format(time.RFC3339Nano) != "2026-01-06T15:42:26.864543Z" {
		t.Errorf("time %v", ev[0].Time)
	}
	if ev[1].Kind != Reward || ev[1].FiatCents != 20 {
		t.Errorf("reward %+v", ev[1])
	}
	if ev[2].Kind != Withdrawal || ev[2].Sats != 100_000 || len(ev[2].Txid) != 64 {
		t.Errorf("withdrawal %+v", ev[2])
	}
}

// Cash App: only the bitcoin rows count; card spending and payments are
// skipped, and so are failed rows. Dates carry US zone abbreviations.
func TestCashApp(t *testing.T) {
	ev := load(t, "cashapp.csv", "cashapp")
	want := []struct {
		k          Kind
		sats, fiat int64
	}{{Buy, 227_272, 100_00}, {Reward, 1_190, 50}, {Withdrawal, 100_000, 0}, {Sell, 100_000, 65_00}, {Deposit, 100_000, 60_00}}
	if len(ev) != len(want) {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for i, w := range want {
		if ev[i].Kind != w.k || ev[i].Sats != w.sats || ev[i].FiatCents != w.fiat {
			t.Errorf("row %d: %+v", i, ev[i])
		}
	}
	if ev[0].FeeCents != 1_50 || ev[0].UID != "cashapp:syn-ca-1" || ev[0].Time.Format(time.RFC3339) != "2024-01-05T14:00:00Z" {
		t.Errorf("buy %+v", ev[0])
	}
	if ev[2].FeeSats != 1_000 || !ev[2].FeeApprox || ev[2].Address != "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq" {
		t.Errorf("withdrawal %+v", ev[2])
	}
	if ev[3].FeeCents != 1_15 || ev[3].Time.Format(time.RFC3339) != "2024-04-15T18:00:00Z" {
		t.Errorf("sale %+v", ev[3])
	}
}

// Some Cash App exports stop at Asset Amount. Synthetic rows.
func TestCashAppShortHeader(t *testing.T) {
	csv := `Transaction ID,Date,Transaction Type,Currency,Amount,Fee,Net Amount,Asset Type,Asset Price,Asset Amount
syn1,2023-06-01 20:00:00 PDT,Bitcoin Buy,USD,-$24.40,-$0.60,-$25.00,BTC,"$27,000.00",0.00090370 BTC
syn2,2023-06-02 20:00:00 PDT,Bitcoin Sale,USD,$24.30,-$0.60,$23.70,BTC,"$26,900.00",0.00090370 BTC`
	h, ev, _, err := Import(strings.NewReader(csv), "")
	if err != nil || h.ID() != "cashapp" || len(ev) != 2 {
		t.Fatalf("%v %v %+v", h, err, ev)
	}
	if ev[0].Kind != Buy || ev[0].FiatCents != 25_00 || ev[0].FeeCents != 60 || ev[0].Sats != 90_370 {
		t.Errorf("buy %+v", ev[0])
	}
	if ev[1].Kind != Sell || ev[1].FiatCents != 24_30 || ev[1].FeeCents != 60 {
		t.Errorf("sale %+v", ev[1])
	}
}

// A spreadsheet of your own, laid out with River's columns, imports as
// River; the Imports page suggests this for exchanges with no importer.
func TestOwnSpreadsheetAsRiver(t *testing.T) {
	csv := `Date,Sent Amount,Sent Currency,Received Amount,Received Currency,Fee Amount,Fee Currency,Tag
2022-03-01,250.00,USD,0.00600000,BTC,2.50,USD,Buy
2022-04-01,0.00500000,BTC,,,0.00001000,BTC,Send
2022-05-01,,,0.00000800,BTC,,,Interest
2022-06-01,0.00100000,BTC,30.00,USD,0.30,USD,Sell`
	h, ev, _, err := Import(strings.NewReader(csv), "")
	if err != nil || h.ID() != "river" {
		t.Fatalf("%v %v", h, err)
	}
	kinds := []Kind{Buy, Withdrawal, Income, Sell}
	if len(ev) != len(kinds) {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for i, k := range kinds {
		if ev[i].Kind != k {
			t.Errorf("row %d: kind %s want %s", i, ev[i].Kind, k)
		}
	}
}

func TestStableUIDs(t *testing.T) {
	// River has no ids: the same row in a longer export gets the same UID.
	short := "Date,Sent Amount,Sent Currency,Received Amount,Received Currency,Fee Amount,Fee Currency,Tag\n" +
		"2023-01-05 10:00:00,100.00,USD,0.00590000,BTC,0.00,USD,Buy\n"
	long := short + "2023-02-05 10:00:00,100.00,USD,0.00490000,BTC,0.00,USD,Buy\n"
	_, a, _, _ := Import(strings.NewReader(short), "river")
	_, b, _, _ := Import(strings.NewReader(long), "river")
	if len(a) != 1 || len(b) != 2 || a[0].UID != b[0].UID || b[0].UID == b[1].UID {
		t.Errorf("uids %v / %v", a, b)
	}
}

func TestParsers(t *testing.T) {
	for in, want := range map[string]int64{"0.00000001": 1, "1": 100_000_000, "-0.5": -50_000_000, "21,000,000": 2_100_000_000_000_000, "(0.1)": -10_000_000} {
		if got, err := Sats(in); err != nil || got != want {
			t.Errorf("Sats(%q) = %d, %v", in, got, err)
		}
	}
	for in, want := range map[string]int64{"$1,234.56": 123456, "0.005": 1, "-0.004": 0, "USD 10": 1000} {
		if got, err := Cents(in); err != nil || got != want {
			t.Errorf("Cents(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParseTime("2024-02-30"); err == nil {
		t.Error("invalid date accepted")
	}
}

func TestUnknownFormat(t *testing.T) {
	f, _ := os.Open("testdata/../importers.go")
	defer f.Close()
	if _, _, _, err := Import(f, ""); err == nil {
		t.Error("expected error for non-CSV input")
	}
}

// A re-export of the same River history with different number and date
// formatting must produce the same row ids, or overlapping imports would
// double count. (River has no reference column, so ids are content hashes.)
func TestRowIDsSurviveReformatting(t *testing.T) {
	a := "Date,Sent Amount,Sent Currency,Received Amount,Received Currency,Fee Amount,Fee Currency,Tag\n2023-01-05 10:00:00,100.00,USD,0.00590000,BTC,0.00,USD,Buy\n"
	b := "Date,Sent Amount,Sent Currency,Received Amount,Received Currency,Fee Amount,Fee Currency,Tag\n2023-01-05T10:00:00Z,100,USD,0.0059,BTC,0,USD,Buy\n"
	_, ea, _, err := Import(strings.NewReader(a), "river")
	if err != nil {
		t.Fatal(err)
	}
	_, eb, _, err := Import(strings.NewReader(b), "river")
	if err != nil {
		t.Fatal(err)
	}
	if len(ea) != 1 || len(eb) != 1 || ea[0].UID != eb[0].UID {
		t.Errorf("ids differ: %q vs %q", ea[0].UID, eb[0].UID)
	}
	c := strings.Replace(b, "0.0059", "0.0060", 1)
	_, ec, _, _ := Import(strings.NewReader(c), "river")
	if ec[0].UID == eb[0].UID {
		t.Error("a different amount got the same id")
	}
}

// The BTC side of a Convert can be either way round. Synthetic rows.
func TestCoinbaseConvertDirection(t *testing.T) {
	csv := `ID,Timestamp,Transaction Type,Asset,Quantity Transacted,Price Currency,Price at Transaction,Subtotal,Total (inclusive of fees and/or spread),Fees and/or Spread,Notes
cv1,2023-01-01 10:00:00 UTC,Convert,BTC,-0.01000000,USD,$20000.00,$200.00,$203.00,$3.00,Converted 0.01 BTC to 0.15 ETH
cv2,2023-02-01 10:00:00 UTC,Convert,BTC,0.00413028,USD,$24213.00,$100.00,$101.50,$1.50,Converted 101.50 USDC to 0.00413028 BTC
cv3,2023-03-01 10:00:00 UTC,Convert,BTC,0.00200000,USD,$25000.00,$50.00,$50.75,$0.75,
cv4,2023-04-01 10:00:00 UTC,Convert,ETH,-0.50000000,USD,$1600.00,$800.00,$812.00,$12.00,Converted 0.5 ETH to 0.04 BTC`
	_, ev, _, err := Import(strings.NewReader(csv), "coinbase")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		k    Kind
		sats int64
	}{{Sell, 1_000_000}, {Buy, 413_028}, {Buy, 200_000}, {Buy, 4_000_000}}
	if len(ev) != len(want) {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for i, w := range want {
		if ev[i].Kind != w.k || ev[i].Sats != w.sats {
			t.Errorf("row %d: %s %d, want %s %d", i, ev[i].Kind, ev[i].Sats, w.k, w.sats)
		}
	}
	if ev[1].FiatCents != 101_50 {
		t.Errorf("convert into BTC basis %d", ev[1].FiatCents)
	}
}

// Re-reading saved rows of a correctly parsed file changes nothing.
func TestReparseIsStable(t *testing.T) {
	for _, f := range []string{"testdata/coinbase.csv", "testdata/fold.csv", "testdata/river.csv", "testdata/strike.csv",
		"testdata/strike-report.csv", "testdata/cashapp.csv", "../trace/testdata/fees/coinbase.csv", "../trace/testdata/fees/fold.csv",
		"../trace/testdata/fees/river.csv", "../trace/testdata/fees/strike.csv"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		h, ev, _, err := Import(strings.NewReader(string(b)), "")
		if err != nil {
			t.Fatal(err)
		}
		raws := make([]map[string]string, len(ev))
		for i, e := range ev {
			raws[i] = e.Raw
		}
		now, err := Reparse(h.ID(), raws)
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range now {
			if e == nil {
				t.Errorf("%s row %d: dropped", f, i)
				continue
			}
			e.UID, e.Row, e.Raw = ev[i].UID, ev[i].Row, ev[i].Raw
			if fmt.Sprintf("%+v", *e) != fmt.Sprintf("%+v", ev[i]) {
				t.Errorf("%s row %d:\n was %+v\n now %+v", f, i, ev[i], *e)
			}
		}
	}
}
