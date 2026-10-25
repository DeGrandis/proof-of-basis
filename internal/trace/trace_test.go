package trace

import (
	"strings"
	"testing"
	"time"

	"btctax/internal/importers"
	"btctax/internal/lots"
	"btctax/internal/scan"
	"btctax/internal/tax"
)

// A synthetic round trip: buy on one exchange, withdraw to cold storage,
// pay a coffee shop, deposit to another exchange, sell there.

func at(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func fixture() ([]ExEvent, []WalletTxs) {
	ex := []ExEvent{
		{"1", "coinbase", importers.Event{Kind: importers.Buy, Time: at("2022-01-01T10:00:00Z"), Sats: 1_000_000, FiatCents: 500_00}},
		{"2", "coinbase", importers.Event{Kind: importers.Withdrawal, Time: at("2022-01-02T10:00:00Z"), Sats: 1_000_000}},
		{"3", "river", importers.Event{Kind: importers.Deposit, Time: at("2023-06-01T12:30:00Z"), Sats: 700_000}},
		{"4", "river", importers.Event{Kind: importers.Sell, Time: at("2023-06-02T00:00:00Z"), Sats: 700_000, FiatCents: 2_000_00, FeeCents: 10_00}},
	}
	cold := WalletTxs{"cold", []scan.Tx{
		{ // receipt from coinbase; network fee paid by coinbase (receives less)
			Txid: "aa", Height: 100, Time: at("2022-01-02T10:20:00Z"),
			Inputs:   []scan.Input{{PrevTxid: "ff"}},
			Outputs:  []scan.Output{{Vout: 0, Address: "cold-0", Sats: 1_000_000, Ours: true}, {Vout: 1, Address: "x", Sats: 5}},
			RecvSats: 1_000_000,
		},
		{ // coffee: 20k to a shop, change back, 1k fee
			Txid: "bb", Height: 200, Time: at("2022-03-01T08:00:00Z"),
			Inputs:   []scan.Input{{PrevTxid: "aa", Sats: 1_000_000, Ours: true}},
			Outputs:  []scan.Output{{Vout: 0, Address: "shop", Sats: 20_000}, {Vout: 1, Address: "cold-1", Sats: 979_000, Ours: true}},
			SentSats: 1_000_000, RecvSats: 979_000, FeeSats: 1_000,
		},
		{ // to river: 700k, change 278k, 1k fee
			Txid: "cc", Height: 300, Time: at("2023-06-01T12:00:00Z"),
			Inputs:   []scan.Input{{PrevTxid: "bb", PrevVout: 1, Sats: 979_000, Ours: true}},
			Outputs:  []scan.Output{{Vout: 0, Address: "river-dep", Sats: 700_000}, {Vout: 1, Address: "cold-2", Sats: 278_000, Ours: true}},
			SentSats: 979_000, RecvSats: 278_000, FeeSats: 1_000,
		},
	}}
	return ex, []WalletTxs{cold}
}

// shift moves the whole fixture by whole years, to test the per-wallet era.
func shift(ex []ExEvent, w []WalletTxs, years int) {
	for i := range ex {
		ex[i].Time = ex[i].Time.AddDate(years, 0, 0)
	}
	for i := range w {
		for j := range w[i].Txs {
			w[i].Txs[j].Time = w[i].Txs[j].Time.AddDate(years, 0, 0)
		}
	}
}

func TestBasisConservedUniversal(t *testing.T) {
	ex, w := fixture()
	r := Build(ex, w, map[string]Label{"tx:bb:0": {Key: "tx:bb:0", Category: Spend, FiatCents: 10_00}}, Options{})
	res, err := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
	if err != nil {
		t.Fatal(err)
	}
	var basis int64
	for _, d := range res.Disposals {
		basis += d.Basis
	}
	for _, l := range res.Open {
		basis += l.Basis
	}
	if basis != 500_00 {
		t.Errorf("basis not conserved: %d", basis)
	}
}

func TestRoundTrip(t *testing.T) {
	ex, w := fixture()
	shift(ex, w, 3) // 2025+: per-wallet tracking, so exact numbers are stable
	r := Build(ex, w, nil, Options{})
	if len(r.Links) != 2 {
		t.Fatalf("links %+v", r.Links)
	}
	if len(r.Unmatched) != 1 || r.Unmatched[0].Key != "tx:bb:0" || r.Unmatched[0].FeeSats != 1_000 {
		t.Fatalf("unmatched %+v", r.Unmatched)
	}

	// Label the coffee as a spend worth $10.
	labels := map[string]Label{"tx:bb:0": {Key: "tx:bb:0", Category: Spend, FiatCents: 10_00}}
	r = Build(ex, w, labels, Options{})
	if len(r.Unmatched) != 0 {
		t.Fatalf("still unmatched %+v", r.Unmatched)
	}
	res, err := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("engine warnings %+v", res.Warnings)
	}
	var proceeds, basis int64
	for _, d := range res.Disposals {
		proceeds += d.Proceeds
		basis += d.Basis
		if d.OriginID != "ex:1" {
			t.Errorf("disposal not traced to the buy: %+v", d)
		}
	}
	// Spend 21k sats (incl. fee) for $10, sell 700k (+1k fee carried) for $1990 net.
	if proceeds != 10_00+1_990_00 {
		t.Errorf("proceeds %d", proceeds)
	}
	// basis: 21k/1M*$500 = $10.50; 701k/1M*$500 = $350.50
	if basis != 10_50+350_50 {
		t.Errorf("basis %d", basis)
	}
	if res.Balances["cold"] != 278_000 || res.Balances["coinbase"] != 0 || res.Balances["river"] != 0 {
		t.Errorf("balances %+v", res.Balances)
	}
}

func TestAmbiguousAmountsNotGuessed(t *testing.T) {
	ex := []ExEvent{
		{"1", "a", importers.Event{Kind: importers.Withdrawal, Time: at("2022-01-02T10:00:00Z"), Sats: 50_000}},
		{"2", "a", importers.Event{Kind: importers.Withdrawal, Time: at("2022-01-02T11:00:00Z"), Sats: 50_000}},
	}
	w := []WalletTxs{{"cold", []scan.Tx{{Txid: "aa", Height: 1, Time: at("2022-01-02T12:00:00Z"),
		Outputs: []scan.Output{{Vout: 0, Sats: 50_000, Ours: true}}, RecvSats: 50_000}}}}
	r := Build(ex, w, nil, Options{})
	if len(r.Links) != 0 || len(r.Unmatched) != 3 {
		t.Errorf("links %+v unmatched %d", r.Links, len(r.Unmatched))
	}
	// The user resolves it manually.
	r = Build(ex, w, map[string]Label{"ex:2": {Key: "ex:2", Category: SelfLink, Counterpart: "tx:aa:0"}}, Options{})
	if len(r.Links) != 1 || r.Links[0].Method != "manual" || len(r.Unmatched) != 1 {
		t.Errorf("manual link %+v unmatched %+v", r.Links, r.Unmatched)
	}
}

func TestInternalTransfer(t *testing.T) {
	w := []WalletTxs{
		{"hot", []scan.Tx{{Txid: "aa", Height: 5, Time: at("2022-01-01T00:00:00Z"),
			Inputs:   []scan.Input{{PrevTxid: "00", Sats: 100_000, Ours: true}},
			Outputs:  []scan.Output{{Vout: 0, Sats: 99_000}},
			SentSats: 100_000, FeeSats: 1_000}}},
		{"cold", []scan.Tx{{Txid: "aa", Height: 5, Time: at("2022-01-01T00:00:00Z"),
			Inputs:   []scan.Input{{PrevTxid: "00"}},
			Outputs:  []scan.Output{{Vout: 0, Sats: 99_000, Ours: true}},
			RecvSats: 99_000}}},
	}
	r := Build(nil, w, nil, Options{})
	if len(r.Unmatched) != 0 || len(r.Ops) != 1 || r.Ops[0].Kind != lots.Transfer || r.Ops[0].FeeSats != 1_000 || r.Ops[0].To != "cold" {
		t.Errorf("ops %+v unmatched %+v", r.Ops, r.Unmatched)
	}
}

func TestWithdrawalFeeIncludedOrNot(t *testing.T) {
	for _, recv := range []int64{498_000, 500_000} {
		ex := []ExEvent{
			{"1", "river", importers.Event{Kind: importers.Buy, Time: at("2025-02-01T00:00:00Z"), Sats: 502_000, FiatCents: 100_00}},
			{"2", "river", importers.Event{Kind: importers.Withdrawal, Time: at("2025-02-02T00:00:00Z"), Sats: 500_000, FeeSats: 2_000}},
		}
		w := []WalletTxs{{"cold", []scan.Tx{{Txid: "aa", Height: 1, Time: at("2025-02-02T01:00:00Z"),
			Outputs: []scan.Output{{Vout: 0, Sats: recv, Ours: true}}, RecvSats: recv}}}}
		r := Build(ex, w, nil, Options{})
		if len(r.Links) != 1 {
			t.Fatalf("recv %d: links %+v", recv, r.Links)
		}
		res, _ := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
		if res.Balances["cold"] != recv || len(res.Warnings) != 0 {
			t.Errorf("recv %d: balances %+v warnings %+v", recv, res.Balances, res.Warnings)
		}
	}
}

// The same wallet added twice must not double count, and its change must
// stay change instead of becoming a transfer between the two copies.
func TestDuplicateWalletIgnored(t *testing.T) {
	ex, w := fixture()
	dup := WalletTxs{"cold copy", w[0].Txs}
	one := Build(ex, w, nil, Options{})
	two := Build(ex, append(w, dup), nil, Options{})
	if len(two.Ops) != len(one.Ops) || len(two.Unmatched) != len(one.Unmatched) || len(two.Links) != len(one.Links) {
		t.Fatalf("ops %d/%d unmatched %d/%d links %d/%d", len(two.Ops), len(one.Ops), len(two.Unmatched), len(one.Unmatched), len(two.Links), len(one.Links))
	}
	for _, op := range two.Ops {
		if op.Account == "cold copy" || op.To == "cold copy" {
			t.Errorf("duplicate wallet used: %+v", op)
		}
	}
	found := false
	for _, w := range two.Warnings {
		found = found || strings.Contains(w, "share addresses")
	}
	if !found {
		t.Errorf("no overlap warning: %v", two.Warnings)
	}
}

// Change that lands in another tracked wallet is a transfer between my own
// wallets; change back to the sender is ignored. Neither needs a label.
func TestChangeToOwnWallets(t *testing.T) {
	tx := func(outs ...scan.Output) scan.Tx {
		return scan.Tx{Txid: "dd", Height: 9, Time: at("2024-02-01T00:00:00Z"),
			Inputs: []scan.Input{{PrevTxid: "00", Sats: 500_000, Ours: true}}, Outputs: outs}
	}
	hot := tx(scan.Output{Vout: 0, Address: "shop", Sats: 100_000}, scan.Output{Vout: 1, Sats: 50_000}, scan.Output{Vout: 2, Address: "hot-1", Sats: 349_000, Ours: true})
	hot.SentSats, hot.RecvSats, hot.FeeSats = 500_000, 349_000, 1_000
	cold := tx(scan.Output{Vout: 0, Sats: 100_000}, scan.Output{Vout: 1, Address: "cold-0", Sats: 50_000, Ours: true}, scan.Output{Vout: 2, Sats: 349_000})
	cold.Inputs[0].Ours, cold.Inputs[0].Sats, cold.RecvSats = false, 0, 50_000
	r := Build(nil, []WalletTxs{{"hot", []scan.Tx{hot}}, {"cold", []scan.Tx{cold}}}, nil, Options{})
	if len(r.Unmatched) != 1 || r.Unmatched[0].Address != "shop" || r.Unmatched[0].FeeSats != 1_000 {
		t.Errorf("unmatched %+v", r.Unmatched)
	}
	if len(r.Ops) != 1 || r.Ops[0].Kind != lots.Transfer || r.Ops[0].Account != "hot" || r.Ops[0].To != "cold" || r.Ops[0].Sats != 50_000 {
		t.Errorf("ops %+v", r.Ops)
	}
}

// Many to one: hot and cold together fund one payment. Each wallet gives
// up exactly what it put in, and the fee is known even though neither
// wallet alone paid for every input.
func TestSpendFromTwoWallets(t *testing.T) {
	when := at("2025-03-01T00:00:00Z")
	outs := []scan.Output{{Vout: 0, Address: "shop", Sats: 140_000}, {Vout: 1, Address: "cold-9", Sats: 59_000}}
	hot := scan.Tx{Txid: "ee", Height: 9, Time: when, Inputs: []scan.Input{{PrevTxid: "h", Sats: 50_000, Ours: true}, {PrevTxid: "c"}}, Outputs: outs, SentSats: 50_000}
	coldOuts := append([]scan.Output(nil), outs...)
	coldOuts[1].Ours = true
	cold := scan.Tx{Txid: "ee", Height: 9, Time: when, Inputs: []scan.Input{{PrevTxid: "h"}, {PrevTxid: "c", Sats: 150_000, Ours: true}}, Outputs: coldOuts, SentSats: 150_000, RecvSats: 59_000}
	ex := []ExEvent{
		{"1", "x", importers.Event{Kind: importers.Buy, Time: at("2025-01-01T00:00:00Z"), Sats: 200_000, FiatCents: 100_00}},
		{"2", "x", importers.Event{Kind: importers.Withdrawal, Time: at("2025-01-02T00:00:00Z"), Sats: 50_000, Txid: "h"}},
		{"3", "x", importers.Event{Kind: importers.Withdrawal, Time: at("2025-01-02T00:00:00Z"), Sats: 150_000, Txid: "c"}},
	}
	recv := func(txid, addr string, sats int64) scan.Tx {
		return scan.Tx{Txid: txid, Height: 5, Time: at("2025-01-02T01:00:00Z"), Outputs: []scan.Output{{Address: addr, Sats: sats, Ours: true}}, RecvSats: sats}
	}
	w := []WalletTxs{{"hot", []scan.Tx{recv("h", "hot-0", 50_000), hot}}, {"cold", []scan.Tx{recv("c", "cold-0", 150_000), cold}}}
	r := Build(ex, w, map[string]Label{"tx:ee:0": {Key: "tx:ee:0", Category: Spend, FiatCents: 70_00}}, Options{})
	if len(r.Unmatched) != 0 {
		t.Fatalf("unmatched %+v", r.Unmatched)
	}
	res, err := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Balances["hot"] != 0 || res.Balances["cold"] != 59_000 {
		t.Errorf("balances %v", res.Balances)
	}
	var spent int64
	for _, d := range res.Disposals {
		spent += d.Sats
		if d.Missing {
			t.Errorf("missing basis: %+v", d)
		}
	}
	if spent != 141_000 { // 140k to the shop plus the 1k fee it carries
		t.Errorf("disposed %d sats: %+v", spent, res.Disposals)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "coinjoin") || strings.Contains(w, "more than one") {
			t.Errorf("unexpected warning %q", w)
		}
	}
}

// Out to a wallet of mine that isn't added, and later back: the coins keep
// their original basis and purchase date, so selling them is long term.
func TestSelfTransferKeepsBasis(t *testing.T) {
	ex := []ExEvent{
		{"1", "x", importers.Event{Kind: importers.Buy, Time: at("2025-01-01T00:00:00Z"), Sats: 100_000, FiatCents: 100_00}},
		{"2", "x", importers.Event{Kind: importers.Withdrawal, Time: at("2025-02-01T00:00:00Z"), Sats: 100_000, Address: "a1"}},
		{"3", "x", importers.Event{Kind: importers.Deposit, Time: at("2026-06-01T00:00:00Z"), Sats: 100_000, Address: "a2"}},
		{"4", "x", importers.Event{Kind: importers.Sell, Time: at("2026-06-02T00:00:00Z"), Sats: 100_000, FiatCents: 300_00}},
	}
	labels := map[string]Label{
		"ex:2": {Key: "ex:2", Category: SelfTransfer},
		"ex:3": {Key: "ex:3", Category: SelfTransfer},
	}
	r := Build(ex, nil, labels, Options{})
	res, err := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unmatched) != 0 || len(res.Disposals) != 1 {
		t.Fatalf("unmatched %+v disposals %+v", r.Unmatched, res.Disposals)
	}
	d := res.Disposals[0]
	if d.Basis != 100_00 || !d.LongTerm || d.Missing || !d.Acquired.Equal(at("2025-01-01T00:00:00Z")) {
		t.Errorf("%+v", d)
	}
}

// With fees treated as small sales, the fee is valued at the day's price.
func TestTransferFeeValued(t *testing.T) {
	_, w := fixture()
	ex := []ExEvent{{"1", "coinbase", importers.Event{Kind: importers.Buy, Time: at("2025-01-01T10:00:00Z"), Sats: 1_000_000, FiatCents: 500_00}}}
	hot := WalletTxs{"hot", []scan.Tx{{Txid: "cc", Height: 300, Time: at("2025-06-01T12:00:00Z"),
		Outputs: []scan.Output{{Vout: 0, Address: "river-dep", Sats: 700_000, Ours: true}}, RecvSats: 700_000}}}
	shift(nil, w, 3) // cold's history moves to 2025
	r := Build(ex, append(w, hot), map[string]Label{
		"tx:aa:0": {Key: "tx:aa:0", Category: Untracked, FiatCents: 500_00},
		"tx:bb:0": {Key: "tx:bb:0", Category: Gift},
	}, Options{Price: func(time.Time) (int64, bool) { return 100_000_00, true }})
	found := false
	for _, op := range r.Ops {
		if op.Kind == lots.Transfer && op.To == "hot" {
			found = true
			if op.FeeSats != 1_000 || op.FeeCents != 1_00 {
				t.Errorf("fee %d sats valued %d cents", op.FeeSats, op.FeeCents)
			}
		}
	}
	if !found {
		t.Errorf("no transfer to hot: %+v", r.Ops)
	}
}

func TestNetworkAndValue(t *testing.T) {
	ex := []ExEvent{
		{"1", "strike", importers.Event{Kind: importers.Withdrawal, Time: at("2024-01-10T00:00:00Z"), Sats: 10_000, Address: "lnbc100n1synthetic"}},
		{"2", "strike", importers.Event{Kind: importers.Withdrawal, Time: at("2024-01-11T00:00:00Z"), Sats: 20_000, Address: "bc1qsynthetic"}},
		{"3", "fold", importers.Event{Kind: importers.Withdrawal, Time: at("2024-01-12T00:00:00Z"), Sats: 30_000, Note: "Lightning withdrawal"}},
		{"4", "fold", importers.Event{Kind: importers.Withdrawal, Time: at("2024-01-13T00:00:00Z"), Sats: 40_000}},
	}
	r := Build(ex, nil, nil, Options{Price: func(time.Time) (int64, bool) { return 50_000_00, true }})
	want := map[string]string{"ex:1": "lightning", "ex:2": "onchain", "ex:3": "lightning", "ex:4": ""}
	for _, it := range r.Unmatched {
		if it.Network != want[it.Key] {
			t.Errorf("%s: network %q, want %q", it.Key, it.Network, want[it.Key])
		}
		if it.Value != it.Sats*50_000_00/100_000_000 {
			t.Errorf("%s: value %d", it.Key, it.Value)
		}
	}
}

// Selling to a person without entering proceeds uses that day's price.
func TestSoldDefaultsToMarketValue(t *testing.T) {
	ex := []ExEvent{
		{"1", "x", importers.Event{Kind: importers.Buy, Time: at("2024-01-01T00:00:00Z"), Sats: 100_000, FiatCents: 40_00}},
		{"2", "x", importers.Event{Kind: importers.Withdrawal, Time: at("2024-02-01T00:00:00Z"), Sats: 100_000, Address: "a"}},
	}
	r := Build(ex, nil, map[string]Label{"ex:2": {Key: "ex:2", Category: Sold}}, Options{Price: func(time.Time) (int64, bool) { return 60_000_00, true }})
	for _, op := range r.Ops {
		if op.ID == "ex:2" && (op.Kind != lots.Dispose || op.Cents != 60_00) {
			t.Errorf("%+v", op)
		}
	}
}

// An exchange withdrawal whose amount includes a network fee it does not
// report, with no txid: in a fee spike the fee (70k) exceeds what arrived
// (20k). The receipt must still link, or spending it drives the wallet
// negative.
func TestWithdrawalWithHiddenFee(t *testing.T) {
	const got, fee = 20_000, 70_000
	ex := []ExEvent{
		{"1", "coinbase", importers.Event{Kind: importers.Buy, Time: at("2017-12-01T10:00:00Z"), Sats: got + fee, FiatCents: 100_00}},
		{"2", "coinbase", importers.Event{Kind: importers.Withdrawal, Time: at("2017-12-20T10:00:00Z"), Sats: got + fee}},
	}
	receipt := func(txid string, sats int64) scan.Tx {
		return scan.Tx{Txid: txid, Height: 100, Time: at("2017-12-20T10:20:00Z"),
			Inputs:   []scan.Input{{PrevTxid: "ff"}},
			Outputs:  []scan.Output{{Vout: 0, Address: "cb-change", Sats: 5_000_000}, {Vout: 1, Address: "cold-0", Sats: sats, Ours: true}},
			RecvSats: sats}
	}
	spend := scan.Tx{Txid: "bb", Height: 200, Time: at("2017-12-21T12:00:00Z"),
		Inputs:   []scan.Input{{PrevTxid: "aa", PrevVout: 1, Sats: got, Ours: true}},
		Outputs:  []scan.Output{{Vout: 0, Address: "shop", Sats: got - 1_000}},
		SentSats: got, FeeSats: 1_000}
	labels := map[string]Label{"tx:bb:0": {Key: "tx:bb:0", Category: Spend, FiatCents: 1_00}}

	r := Build(ex, []WalletTxs{{"cold", []scan.Tx{receipt("aa", got), spend}}}, labels, Options{})
	if len(r.Links) != 1 || r.Links[0].To != "tx:aa:1" || len(r.Unmatched) != 0 {
		t.Fatalf("links %+v unmatched %+v", r.Links, r.Unmatched)
	}
	res, err := lots.Run(r.Ops, lots.Config{Rules: tax.US{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Balances["coinbase"] != 0 || res.Balances["cold"] != 0 {
		t.Errorf("balances %v", res.Balances)
	}
	for _, op := range r.Ops {
		if op.ID == "ex:2>tx:aa:1" && (op.Sats != got || op.FeeSats != fee) {
			t.Errorf("transfer %+v", op)
		}
	}

	// Two receipts that could both be it: no guess.
	r = Build(ex, []WalletTxs{{"cold", []scan.Tx{receipt("aa", got), receipt("cc", got+5_000)}}}, nil, Options{})
	if len(r.Links) != 0 {
		t.Errorf("ambiguous receipts linked: %+v", r.Links)
	}
}
