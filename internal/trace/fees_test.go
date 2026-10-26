package trace

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/importers"
	"github.com/DeGrandis/proof-of-basis/internal/lots"
	"github.com/DeGrandis/proof-of-basis/internal/scan"
	"github.com/DeGrandis/proof-of-basis/internal/tax"
)

// Withdrawals to your own wallet, every way an export can describe the
// network fee: paid by the exchange, reported in BTC on top of or inside
// the amount, reported only in USD, or folded into the amount unreported.
// The CSVs in testdata/fees are synthetic. Each withdrawal lands in the
// tracked wallet "cold"; the chain receipt is the truth for what arrived,
// and every balance below is worked out by hand from the fixtures.

// strikeTxid is the Transaction Hash on the first Strike send.
const strikeTxid = "5151515151515151515151515151515151515151515151515151515151515151"

// receipts: what reached the wallet for each withdrawal, keyed by the
// receiving address. The time is the withdrawal's plus 20 minutes.
var feeReceipts = []struct {
	addr string
	at   string
	sats int64
	txid string
}{
	{"cold-c1", "2017-11-10T10:20:00Z", 1_000_000, ""},       // exchange paid the fee
	{"cold-c2", "2017-12-20T10:20:00Z", 20_000, ""},          // 70,000 fee folded in, unreported
	{"cold-c3", "2018-01-10T10:20:00Z", 2_000_000, ""},       // $3.00 = 20,000 sats on top
	{"cold-c4", "2018-02-10T10:20:00Z", 2_992_660, ""},       // 7,340 sats inside, reported as $3.17
	{"cold-r1", "2023-01-10T10:20:00Z", 1_000_000, ""},       // exchange paid
	{"cold-r2", "2023-01-20T10:20:00Z", 1_000_000, ""},       // 2,000 on top
	{"cold-r3", "2023-01-30T10:20:00Z", 998_000, ""},         // 2,000 inside
	{"cold-s1", "2024-01-10T09:20:00Z", 500_000, strikeTxid}, // 1,500 on top
	{"cold-s2", "2024-01-20T09:20:00Z", 498_500, ""},         // 1,500 inside
	{"cold-s3", "2024-01-30T09:20:00Z", 200_000, ""},         // exchange paid
	{"cold-f1", "2025-02-10T10:20:00Z", 300_000, ""},         // exchange paid
	{"cold-f2", "2025-02-20T10:20:00Z", 300_000, ""},         // $1.50 = 3,000 on top
}

func feeFixture(t *testing.T) ([]ExEvent, []WalletTxs) {
	t.Helper()
	var ex []ExEvent
	for _, f := range []string{"coinbase", "river", "strike", "fold"} {
		r, err := os.Open("testdata/fees/" + f + ".csv")
		if err != nil {
			t.Fatal(err)
		}
		h, evs, warns, err := importers.Import(r, "")
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if h.ID() != f {
			t.Fatalf("%s detected as %s", f, h.ID())
		}
		for _, w := range warns {
			t.Logf("%s line %d: %s", f, w.Row, w.Message)
		}
		for i, e := range evs {
			ex = append(ex, ExEvent{fmt.Sprintf("%s-%d", f, i), f, e})
		}
	}
	var txs []scan.Tx
	for i, r := range feeReceipts {
		txid := r.txid
		if txid == "" {
			txid = fmt.Sprintf("%064x", i+1)
		}
		txs = append(txs, scan.Tx{Txid: txid, Height: int64(100 + i), Time: at(r.at),
			Inputs:   []scan.Input{{PrevTxid: "exchange-hot-wallet"}},
			Outputs:  []scan.Output{{Vout: 0, Address: "exchange-change", Sats: 7_000_000}, {Vout: 1, Address: r.addr, Sats: r.sats, Ours: true}},
			RecvSats: r.sats})
	}
	// cold pays River 500,000 from the first receipt, 2,000 fee, 498,000 change.
	first := txs[0].Txid
	txs = append(txs, scan.Tx{Txid: fmt.Sprintf("%064x", 999), Height: 500, Time: at("2023-02-10T12:40:00Z"),
		Inputs:   []scan.Input{{PrevTxid: first, PrevVout: 1, Sats: 1_000_000, Ours: true}},
		Outputs:  []scan.Output{{Vout: 0, Address: "river-deposit", Sats: 500_000}, {Vout: 1, Address: "cold-change", Sats: 498_000, Ours: true}},
		SentSats: 1_000_000, RecvSats: 498_000, FeeSats: 2_000})
	return ex, []WalletTxs{{"cold", txs}}
}

func TestWithdrawalFees(t *testing.T) {
	ex, w := feeFixture(t)
	// The Coinbase payment to a merchant (not a wallet of ours) is a spend.
	var merchant string
	for _, e := range ex {
		if e.Account == "coinbase" && e.Kind == importers.Withdrawal && e.Address == "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq" {
			merchant = "ex:" + e.ID
		}
	}
	labels := map[string]Label{merchant: {Key: merchant, Category: Spend, FiatCents: 10_00}}
	r := Build(ex, w, labels, Options{})
	for _, it := range r.Unmatched {
		t.Errorf("unmatched %s %s %d sats (fee %d)", it.Key, it.Account, it.Sats, it.FeeSats)
	}
	res, err := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		// 10,000,000 bought; out 1,000,000 + 90,000 + 2,020,000 + 3,000,000 + 125,000
		"coinbase": 3_765_000,
		// 5,000,000 bought; out 1,000,000 + 1,002,000 + 1,000,000; in 500,000
		"river": 2_498_000,
		// 2,000,000 bought; out 501,500 + 500,000 + 200,000
		"strike": 798_500,
		// 1,000,000 bought; out 300,000 + 303,000
		"fold": 397_000,
		// every receipt (10,809,160), less 500,000 sent to River and its 2,000 fee
		"cold": 10_307_160,
	}
	for acct, sats := range want {
		if got := res.Balances[acct]; got != sats {
			t.Errorf("%s balance %d, want %d (off by %d)", acct, got, sats, got-sats)
		}
	}
	// Fees per transfer, keyed by the receiving address.
	wantFee := map[string]int64{"cold-c1": 0, "cold-c2": 70_000, "cold-c3": 20_000, "cold-c4": 7_340,
		"cold-r1": 0, "cold-r2": 2_000, "cold-r3": 2_000, "cold-s1": 1_500, "cold-s2": 1_500, "cold-s3": 0,
		"cold-f1": 0, "cold-f2": 3_000, "river-deposit": 2_000}
	fees := map[string]int64{}
	for _, l := range r.Links {
		addr := r.Items[l.To].Address
		if addr == "" {
			addr = r.Items[l.From].Address
		}
		fees[addr] = l.FeeSats
	}
	for addr, f := range wantFee {
		if got, ok := fees[addr]; !ok {
			t.Errorf("%s: not linked", addr)
		} else if got != f {
			t.Errorf("%s: fee %d, want %d", addr, got, f)
		}
	}
}

// In the 2017 fee spike an exchange's send could wait days for a block.
// The receipt still links (by address, amount, or amount less a hidden
// fee) up to two weeks out, and the wallet never goes negative.
func TestSlowConfirmingWithdrawal(t *testing.T) {
	sent := at("2017-12-20T10:00:00Z")
	build := func(delay time.Duration, sats int64, addr string, extra ...ExEvent) (*Result, *lots.Result) {
		ex := append([]ExEvent{
			{"1", "coinbase", importers.Event{Kind: importers.Buy, Time: at("2017-12-01T10:00:00Z"), Sats: 200_000, FiatCents: 100_00}},
			{"2", "coinbase", importers.Event{Kind: importers.Withdrawal, Time: sent, Sats: sats, Address: addr}},
		}, extra...)
		got := sent.Add(delay)
		cold := WalletTxs{"cold", []scan.Tx{
			{Txid: "aa", Height: 100, Time: got, Inputs: []scan.Input{{PrevTxid: "exchange-hot-wallet"}},
				Outputs:  []scan.Output{{Vout: 0, Address: "exchange-change", Sats: 30_000}, {Vout: 1, Address: "cold-0", Sats: 20_000, Ours: true}},
				RecvSats: 20_000},
			{Txid: "bb", Height: 200, Time: got.Add(48 * time.Hour),
				Inputs:  []scan.Input{{PrevTxid: "aa", PrevVout: 1, Sats: 20_000, Ours: true}},
				Outputs: []scan.Output{{Vout: 0, Address: "shop", Sats: 19_000}}, SentSats: 20_000, FeeSats: 1_000},
		}}
		r := Build(ex, []WalletTxs{cold}, map[string]Label{"tx:bb:0": {Key: "tx:bb:0", Category: Spend, FiatCents: 1_00}}, Options{})
		res, err := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
		if err != nil {
			t.Fatal(err)
		}
		return r, res
	}
	for _, delay := range []time.Duration{20 * time.Minute, 5 * 24 * time.Hour, 13 * 24 * time.Hour} {
		for _, c := range []struct {
			sats, coinbase int64 // exported amount, coinbase balance after
			addr           string
		}{
			{20_000, 180_000, ""},       // exchange paid the fee
			{20_000, 180_000, "cold-0"}, // ... and the export names the address
			{90_000, 110_000, ""},       // 70,000 fee folded into the amount
		} {
			r, res := build(delay, c.sats, c.addr)
			if len(r.Links) != 1 || res.Balances["cold"] != 0 || res.Balances["coinbase"] != c.coinbase {
				t.Errorf("delay %v, %+v: links %+v balances %v", delay, c, r.Links, res.Balances)
			}
		}
	}
	// Past two weeks it is left for Review rather than guessed.
	if r, _ := build(20*24*time.Hour, 20_000, ""); len(r.Links) != 0 {
		t.Errorf("linked a receipt 20 days late: %+v", r.Links)
	}
	// Two sends of the same amount could each be the slow one: no guess.
	twin := ExEvent{"3", "coinbase", importers.Event{Kind: importers.Withdrawal, Time: sent.Add(-24 * time.Hour), Sats: 20_000}}
	if r, _ := build(5*24*time.Hour, 20_000, "", twin); len(r.Links) != 0 {
		t.Errorf("ambiguous slow receipt linked: %+v", r.Links)
	}
}
