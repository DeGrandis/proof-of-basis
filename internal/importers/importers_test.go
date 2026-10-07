package importers

import (
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
	}{{Buy, 590_000, 100_00}, {Withdrawal, 500_000, 0}, {Income, 500, 0}, {Sell, 90_000, 55_00}}
	for i, w := range want {
		if ev[i].Kind != w.k || ev[i].Sats != w.sats || ev[i].FiatCents != w.fiat {
			t.Errorf("row %d: %+v", i, ev[i])
		}
	}
	if ev[1].FeeSats != 2_000 {
		t.Errorf("withdrawal fee %d", ev[1].FeeSats)
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
