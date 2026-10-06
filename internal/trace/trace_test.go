package trace

import (
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
