package lots

import (
	"testing"
	"time"

	"btctax/internal/tax"
)

// All scenarios use invented amounts and dates.

func d(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

const btc = 100_000_000

// perAcct makes every scenario use per-account tracking from the start.
type perAcct struct{ tax.US }

func (perAcct) PerAccountFrom() time.Time { return time.Time{} }

func run(t *testing.T, ops []Op, m tax.Method, rules tax.Rules) *Result {
	t.Helper()
	if rules == nil {
		rules = perAcct{}
	}
	r, err := Run(ops, Config{Rules: rules, Method: func(int) tax.Method { return m }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func gains(r *Result) (short, long int64) {
	for _, d := range r.Disposals {
		if d.LongTerm {
			long += d.Gain()
		} else {
			short += d.Gain()
		}
	}
	return
}

func TestFIFOvsLIFO(t *testing.T) {
	ops := []Op{
		{ID: "b1", Kind: Acquire, Time: d("2021-01-10"), Account: "ex", Sats: btc, Cents: 30_000_00},
		{ID: "b2", Kind: Acquire, Time: d("2022-06-01"), Account: "ex", Sats: btc, Cents: 20_000_00},
		{ID: "s1", Kind: Dispose, Time: d("2022-07-01"), Account: "ex", Sats: btc / 2, Cents: 10_000_00, FeeCents: 50_00},
	}
	r := run(t, ops, tax.FIFO, nil)
	if s, l := gains(r); s != 0 || l != 9_950_00-15_000_00 {
		t.Errorf("fifo short %d long %d", s, l)
	}
	r = run(t, ops, tax.LIFO, nil)
	if s, l := gains(r); l != 0 || s != 9_950_00-10_000_00 {
		t.Errorf("lifo short %d long %d", s, l)
	}
}

func TestLongTermBoundary(t *testing.T) {
	us := tax.US{}
	if us.LongTerm(d("2023-03-15"), d("2024-03-15")) {
		t.Error("anniversary should be short-term")
	}
	if !us.LongTerm(d("2023-03-15"), d("2024-03-16")) {
		t.Error("day after anniversary should be long-term")
	}
}

func TestTransferKeepsAcquisitionAndCarriesFee(t *testing.T) {
	ops := []Op{
		{ID: "b1", Kind: Acquire, Time: d("2021-01-10"), Account: "river", Sats: btc, Cents: 30_000_00},
		{ID: "w1", Kind: Transfer, Time: d("2021-02-01"), Account: "river", To: "cold", Sats: btc - 10_000, FeeSats: 10_000},
		{ID: "d1", Kind: Transfer, Time: d("2023-05-01"), Account: "cold", To: "coinbase", Sats: btc - 20_000, FeeSats: 10_000},
		{ID: "s1", Kind: Dispose, Time: d("2023-05-02"), Account: "coinbase", Sats: btc - 20_000, Cents: 28_000_00},
	}
	r := run(t, ops, tax.FIFO, nil)
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals %+v", r.Disposals)
	}
	got := r.Disposals[0]
	if !got.LongTerm || got.OriginID != "b1" || got.Basis != 30_000_00 || got.Missing {
		t.Errorf("disposal %+v", got)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings %+v", r.Warnings)
	}
	if r.Balances["coinbase"] != 0 || r.Balances["river"] != 0 || r.Balances["cold"] != 0 {
		t.Errorf("balances %+v", r.Balances)
	}
}

func TestFeeAsDisposal(t *testing.T) {
	ops := []Op{
		{ID: "b1", Kind: Acquire, Time: d("2021-01-10"), Account: "a", Sats: btc, Cents: 10_000_00},
		{ID: "t1", Kind: Transfer, Time: d("2021-03-01"), Account: "a", To: "b", Sats: btc - 1_000_000, FeeSats: 1_000_000, FeeCents: 500_00},
	}
	r, _ := Run(ops, Config{Rules: perAcct{}, FeeMode: FeeAsDisposal})
	if len(r.Disposals) != 1 || r.Disposals[0].Basis != 100_00 || r.Disposals[0].Proceeds != 500_00 {
		t.Fatalf("fee disposal %+v", r.Disposals)
	}
	if len(r.Open) != 1 || r.Open[0].Basis != 9_900_00 || r.Open[0].Account != "b" {
		t.Errorf("open %+v", r.Open)
	}
}

func TestSpecificID(t *testing.T) {
	ops := []Op{
		{ID: "b1", Kind: Acquire, Time: d("2021-01-10"), Account: "ex", Sats: btc, Cents: 30_000_00},
		{ID: "b2", Kind: Acquire, Time: d("2021-06-01"), Account: "ex", Sats: btc, Cents: 60_000_00},
		{ID: "s1", Kind: Dispose, Time: d("2021-07-01"), Account: "ex", Sats: btc, Cents: 35_000_00, Picks: []Pick{{"b2", btc}}},
	}
	r := run(t, ops, tax.Specific, nil)
	if len(r.Disposals) != 1 || r.Disposals[0].LotID != "b2" || r.Disposals[0].Gain() != -25_000_00 {
		t.Errorf("%+v", r.Disposals)
	}
}

func TestShortfallFlagged(t *testing.T) {
	ops := []Op{
		{ID: "b1", Kind: Acquire, Time: d("2021-01-10"), Account: "ex", Sats: btc / 2, Cents: 10_000_00},
		{ID: "s1", Kind: Dispose, Time: d("2021-07-01"), Account: "ex", Sats: btc, Cents: 40_000_00},
	}
	r := run(t, ops, tax.FIFO, nil)
	if len(r.Disposals) != 2 || !r.Disposals[1].Missing || r.Disposals[1].Basis != 0 {
		t.Errorf("%+v", r.Disposals)
	}
	if len(r.Warnings) != 1 {
		t.Errorf("warnings %+v", r.Warnings)
	}
	if r.Disposals[0].Proceeds+r.Disposals[1].Proceeds != 40_000_00 {
		t.Error("proceeds not conserved")
	}
}

func TestUniversalThenPerAccount(t *testing.T) {
	ops := []Op{
		{ID: "b1", Kind: Acquire, Time: d("2023-01-10"), Account: "a", Sats: btc, Cents: 20_000_00},
		{ID: "b2", Kind: Acquire, Time: d("2023-02-10"), Account: "b", Sats: btc, Cents: 25_000_00},
		// Universal FIFO: selling from account b uses b1's basis.
		{ID: "s1", Kind: Dispose, Time: d("2024-06-01"), Account: "b", Sats: btc / 2, Cents: 30_000_00},
		// After 2025 the pool is split by actual balances.
		{ID: "s2", Kind: Dispose, Time: d("2025-03-01"), Account: "b", Sats: btc / 2, Cents: 40_000_00},
	}
	r := run(t, ops, tax.FIFO, tax.US{})
	if r.Disposals[0].OriginID != "b1" {
		t.Errorf("universal FIFO should use b1, got %+v", r.Disposals[0])
	}
	if len(r.Allocations) == 0 {
		t.Fatal("expected a transition allocation")
	}
	// Account a (balance 1 BTC) gets the oldest remaining: rest of b1 then part of b2.
	if r.Allocations[0].Account != "a" {
		t.Errorf("allocations %+v", r.Allocations)
	}
	last := r.Disposals[len(r.Disposals)-1]
	if last.Account != "b" || last.OriginID != "b2" || last.Missing {
		t.Errorf("post-transition disposal %+v", last)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings %+v", r.Warnings)
	}
}

func TestMulDivNoOverflow(t *testing.T) {
	if got := mulDiv(9_000_000_000_00, 2_100_000_000_000_000, 2_100_000_000_000_000); got != 9_000_000_000_00 {
		t.Error(got)
	}
	if got := mulDiv(1_000_000_000_000, 1_999_999_999_999_999, 2_100_000_000_000_000); got <= 0 {
		t.Error(got)
	}
}

func TestRemove(t *testing.T) {
	ops := []Op{
		{ID: "b1", Kind: Acquire, Time: d("2021-01-10"), Account: "w", Sats: btc, Cents: 10_000_00},
		{ID: "g1", Kind: Remove, Time: d("2021-02-01"), Account: "w", Sats: btc / 4},
	}
	r := run(t, ops, tax.FIFO, nil)
	if len(r.Disposals) != 0 || len(r.Removals) != 1 || r.Removals[0].Basis != 2_500_00 || r.Balances["w"] != btc*3/4 {
		t.Errorf("%+v", r)
	}
}
