package app

import (
	"sort"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lots"
	"github.com/DeGrandis/proof-of-basis/internal/trace"
)

// lossLimit is how much net capital loss a year can deduct from other
// income, by filing status; the rest carries forward (Schedule D).
var lossLimit = map[string]int64{"single": 3000_00, "mfj": 3000_00, "hoh": 3000_00, "mfs": 1500_00}

// carry is capital loss brought forward from earlier years, by character,
// as positive amounts.
type carry struct{ short, long int64 }

// netYear applies a carryover and the yearly limit to a year's short- and
// long-term results, the way Schedule D nets them (simplified): losses of
// one term offset gains of the other, then up to limit of net loss is
// deducted from other income, short-term loss first, and the rest carries
// forward keeping its character. It returns the taxable gains, the loss
// deducted and the carryover out.
func netYear(st, lt int64, in carry, limit int64) (tst, tlt, deducted int64, out carry) {
	st -= in.short
	lt -= in.long
	// What is left after netting keeps the character of the larger side.
	if (st < 0 && lt > 0) || (lt < 0 && st > 0) {
		if net := st + lt; (net >= 0) == (st > 0) {
			st, lt = net, 0
		} else {
			st, lt = 0, net
		}
	}
	if st >= 0 && lt >= 0 {
		return st, lt, 0, carry{}
	}
	sl, ll := max(-st, 0), max(-lt, 0)
	deducted = min(sl+ll, limit)
	useS := min(sl, deducted)
	sl -= useS
	ll -= deducted - useS
	return max(st, 0), max(lt, 0), deducted, carry{sl, ll}
}

// loc is the user's time zone: tax years and holding periods follow their
// calendar, not UTC.
func loc(set map[string]string) *time.Location {
	if l, err := time.LoadLocation(set["timezone"]); err == nil {
		return l
	}
	return time.UTC
}

// summarize builds the per-year results: gains by term, income, the loss
// limit and carryover, and an impact estimate at the user's rates. Every
// calendar year from the first record to through is included, since a
// carried loss keeps being deducted in years without any activity.
func summarize(disps []lots.Disposal, income []trace.IncomeRow, set map[string]string, through int) []YearSummary {
	years := map[int]*YearSummary{}
	y := func(yr int) *YearSummary {
		if years[yr] == nil {
			years[yr] = &YearSummary{Year: yr}
		}
		return years[yr]
	}
	for _, d := range disps {
		ys := y(d.Disposed.Year())
		if d.LongTerm {
			ys.LongProceeds += d.Proceeds
			ys.LongBasis += d.Basis
		} else {
			ys.ShortProceeds += d.Proceeds
			ys.ShortBasis += d.Basis
		}
		if d.Missing {
			ys.MissingBasis += d.Sats
		}
	}
	for _, in := range income {
		y(in.Time.Year()).Income += in.Cents
	}
	if len(years) == 0 {
		return []YearSummary{}
	}
	first := through
	for yr := range years {
		first = min(first, yr)
	}
	limit, ok := lossLimit[set["filingStatus"]]
	if !ok {
		limit = lossLimit["single"]
	}
	sr, lr := rateBP(set, "rateShort"), rateBP(set, "rateLong")
	var c carry
	var out []YearSummary
	for yr := first; yr <= max(through, first); yr++ {
		ys := years[yr]
		if ys == nil {
			if c == (carry{}) {
				continue
			}
			ys = &YearSummary{Year: yr}
		}
		ys.CarryInShort, ys.CarryInLong = c.short, c.long
		tst, tlt, ded, next := netYear(ys.ShortProceeds-ys.ShortBasis, ys.LongProceeds-ys.LongBasis, c, limit)
		ys.LossDeducted, ys.CarryOutShort, ys.CarryOutLong = ded, next.short, next.long
		// Gains at their rates, income as ordinary, and the deducted loss
		// lowering ordinary income: negative means the year lowers tax.
		ys.EstimatedTax = lots.MulDiv(tst+ys.Income, sr, 10000) + lots.MulDiv(tlt, lr, 10000) - lots.MulDiv(ded, sr, 10000)
		c = next
		out = append(out, *ys)
	}
	// Years after through only matter if they had activity (clock skew, or
	// records dated in the future): keep them too.
	for yr, ys := range years {
		if yr > max(through, first) {
			out = append(out, *ys)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year < out[j].Year })
	return out
}
