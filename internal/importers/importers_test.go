package importers

import (
	"os"
	"testing"
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
	if len(ev) != 4 {
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
		t.Errorf("income %+v", ev[3])
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
	}{{Buy, 590_000, 100_00}, {Withdrawal, 498_000, 0}, {Income, 500, 0}, {Sell, 90_000, 55_00}}
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
	if len(ev) != 2 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	if ev[0].Kind != Buy || ev[0].Sats != 560_000 || ev[0].FiatCents != 250_00 {
		t.Errorf("buy %+v", ev[0])
	}
	if ev[1].Kind != Withdrawal || ev[1].FeeSats != 1_500 || ev[1].Address == "" {
		t.Errorf("withdrawal %+v", ev[1])
	}
	if ev[0].Time.Format("2006-01-02 15:04:05") != "2024-01-05 14:00:01" {
		t.Errorf("time %v", ev[0].Time)
	}
}

func TestFold(t *testing.T) {
	ev := load(t, "fold.csv", "fold")
	if len(ev) != 3 {
		t.Fatalf("got %d events", len(ev))
	}
	if ev[0].Kind != Buy || ev[0].FiatCents != 101_00 {
		t.Errorf("buy %+v", ev[0])
	}
	if ev[1].Kind != Reward || ev[1].FiatCents != 20 {
		t.Errorf("reward %+v", ev[1])
	}
	if ev[2].Kind != Withdrawal || ev[2].Sats != 100_000 {
		t.Errorf("withdrawal %+v", ev[2])
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
