package app

import (
	"testing"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lots"
	"github.com/DeGrandis/proof-of-basis/internal/trace"
)

// Capital losses net across terms, deduct up to the limit from other
// income, and carry forward with their character, through quiet years.
func TestLossCarryforward(t *testing.T) {
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	disps := []lots.Disposal{
		// 2021: a $10,000 short-term loss and a $1,000 long-term gain.
		{Disposed: at("2021-03-01"), Proceeds: 0, Basis: 10_000_00},
		{Disposed: at("2021-04-01"), Proceeds: 2_000_00, Basis: 1_000_00, LongTerm: true},
		// 2024: a $5,000 long-term gain soaks up what is left.
		{Disposed: at("2024-05-01"), Proceeds: 6_000_00, Basis: 1_000_00, LongTerm: true},
	}
	set := map[string]string{"filingStatus": "single", "rateShort": "24", "rateLong": "15"}
	ys := summarize(disps, []trace.IncomeRow{}, set, 2024)
	by := map[int]YearSummary{}
	for _, y := range ys {
		by[y.Year] = y
	}
	// 2021: net -9,000; 3,000 deducted; 6,000 short-term carries.
	if y := by[2021]; y.LossDeducted != 3000_00 || y.CarryOutShort != 6000_00 || y.CarryOutLong != 0 || y.EstimatedTax != -720_00 {
		t.Errorf("2021 %+v", y)
	}
	// 2022 and 2023 had no sales but still deduct 3,000 each.
	if y := by[2022]; y.CarryInShort != 6000_00 || y.LossDeducted != 3000_00 || y.CarryOutShort != 3000_00 {
		t.Errorf("2022 %+v", y)
	}
	if y := by[2023]; y.LossDeducted != 3000_00 || y.CarryOutShort != 0 {
		t.Errorf("2023 %+v", y)
	}
	// 2024: nothing left to carry; the gain is taxed in full.
	if y := by[2024]; y.CarryInShort != 0 || y.EstimatedTax != 750_00 {
		t.Errorf("2024 %+v", y)
	}
	// Married filing separately: $1,500 a year.
	set["filingStatus"] = "mfs"
	if y := summarize(disps, nil, set, 2021)[0]; y.LossDeducted != 1500_00 || y.CarryOutShort != 7500_00 {
		t.Errorf("mfs %+v", y)
	}
}

func TestNetYear(t *testing.T) {
	for _, c := range []struct {
		st, lt        int64
		in            carry
		tst, tlt, ded int64
		out           carry
	}{
		{100, 50, carry{}, 100, 50, 0, carry{}},
		{-100, 50, carry{}, 0, 0, 50, carry{}}, // net -50, all deducted
		{100, -5000_00, carry{}, 0, 0, 3000_00, carry{0, 5000_00 - 100 - 3000_00}},
		{0, 0, carry{1000_00, 4000_00}, 0, 0, 3000_00, carry{0, 2000_00}}, // short used first
		{5000_00, 0, carry{1000_00, 0}, 4000_00, 0, 0, carry{}},
	} {
		tst, tlt, ded, out := netYear(c.st, c.lt, c.in, 3000_00)
		if tst != c.tst || tlt != c.tlt || ded != c.ded || out != c.out {
			t.Errorf("%+v: got %d %d %d %+v", c, tst, tlt, ded, out)
		}
	}
}
