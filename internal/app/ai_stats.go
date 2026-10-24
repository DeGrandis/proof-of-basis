package app

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"btctax/internal/ollama"
)

// analyze_records: one tool for totals and statistics over the user's
// records, so "most common buy amount", "average price paid per year",
// "which weekday do I buy on" and the like need no extra tools. The server
// does the math; the model only picks what to measure.

func analyzeToolSpec() ollama.Tool {
	return ollama.Tool{Type: "function", Function: ollama.ToolSpec{
		Name: "analyze_records",
		Description: "Exact totals and statistics over records. Pick the records (source and filters), what to measure, and the statistic; optionally group. " +
			"Use it for any count, total, average, median, most common value, largest/smallest, distribution or per-year/per-account question instead of working it out yourself. " +
			"Examples: most common buy amount = source buys, measure usd, stat mode; average price paid per year = source buys, measure price, stat mean, group_by year; " +
			"usual holding time before selling = source sales, measure days_held, stat median; which weekday I buy on = source buys, measure weekday, stat count.",
		Parameters: obj(map[string]any{
			"source":   str("buys (default), sales (one row per sale), records (every record), income, review (items waiting for a label), lots (coins still held)"),
			"measure":  str("usd (default), btc, price (USD per BTC), fee (USD), fee_btc, gain (USD; sales and lots), days_held (sales and lots), or a category: weekday, month, year, account, kind"),
			"stat":     str("summary (default: count, sum, mean, median, min, max), count, sum, mean, median, mode (most common), min, max, stddev, percentiles, histogram, top (largest N records), bottom (smallest N)"),
			"group_by": str("none (default), year, month, weekday, account or kind"),
			"from":     str("first day, YYYY-MM-DD (optional)"),
			"to":       str("last day, YYYY-MM-DD (optional)"),
			"account":  str("only this account (optional)"),
			"kind":     str("only this kind, for source records (optional)"),
			"min":      num("only rows whose measure is at least this (optional)"),
			"max":      num("only rows whose measure is at most this (optional)"),
			"round_to": num("round values to this step first, for mode and histogram bins (for example 10 for $10 buckets); chosen automatically when left out"),
			"top_n":    num("how many rows for top/bottom, default 10"),
		}),
	}}
}

type statRow struct {
	key, acct, kind string
	t               time.Time
	usd, btc        float64
	hasUSD          bool
	fee, feeBTC     float64
	gain, days      float64
	hasGain         bool
}

var weekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

func (tb *toolbox) statRows(src string) ([]statRow, string, error) {
	var rows []statRow
	switch src {
	case "", "buys", "records":
		for _, g := range tb.rep.Graph {
			if src != "records" && g.Kind != "buy" && g.Kind != "purchase" {
				continue
			}
			rows = append(rows, statRow{key: g.Key, acct: g.Account, kind: g.Kind, t: time.Unix(g.Time, 0).UTC(),
				usd: float64(g.Cents) / 100, btc: float64(g.Sats) / 1e8, hasUSD: g.Cents != 0, fee: float64(g.FeeCents) / 100, feeBTC: float64(g.FeeSats) / 1e8})
		}
		if src == "" {
			src = "buys"
		}
	case "sales":
		idx := map[string]int{}
		for _, d := range tb.rep.Disposals {
			i, ok := idx[d.OpID]
			if !ok {
				i = len(rows)
				idx[d.OpID] = i
				rows = append(rows, statRow{key: d.OpID, acct: d.Account, kind: "sale", t: d.Disposed.UTC(), hasUSD: true, hasGain: true})
			}
			r := &rows[i]
			btc := float64(d.Sats) / 1e8
			// days held, weighted by amount across the sale's lots
			r.days = (r.days*r.btc + d.Disposed.Sub(d.Acquired).Hours()/24*btc) / math.Max(r.btc+btc, 1e-12)
			r.btc += btc
			r.usd += float64(d.Proceeds) / 100
			r.gain += float64(d.Gain()) / 100
		}
	case "income":
		for _, x := range tb.rep.Income {
			rows = append(rows, statRow{key: x.Key, kind: x.Kind, t: x.Time.UTC(), usd: float64(x.Cents) / 100, btc: float64(x.Sats) / 1e8, hasUSD: true})
		}
	case "review":
		for _, it := range tb.rep.Unmatched {
			rows = append(rows, statRow{key: it.Key, acct: it.Account, kind: string(it.Dir), t: it.Time.UTC(), usd: float64(it.Value) / 100, btc: float64(it.Sats) / 1e8, hasUSD: it.Value != 0, feeBTC: float64(it.FeeSats) / 1e8})
		}
	case "lots":
		now := time.Now()
		for _, l := range tb.rep.OpenLots {
			if l.Sats <= 0 {
				continue
			}
			worth := float64(l.Sats) / 1e8 * float64(tb.rep.Price) / 100
			rows = append(rows, statRow{key: l.ID, acct: l.Account, kind: "lot", t: l.Acquired.UTC(), usd: float64(l.Basis) / 100, btc: float64(l.Sats) / 1e8, hasUSD: true,
				days: now.Sub(l.Acquired).Hours() / 24, gain: worth - float64(l.Basis)/100, hasGain: tb.rep.Price > 0})
		}
	default:
		return nil, "", errors.New("source must be buys, sales, records, income, review or lots")
	}
	return rows, src, nil
}

// value reads the measure from a row; ok is false when the row has none.
func statValue(r statRow, m string) (v float64, cat string, ok bool) {
	switch m {
	case "", "usd":
		return r.usd, "", r.hasUSD
	case "btc":
		return r.btc, "", r.btc != 0
	case "price":
		if r.hasUSD && r.btc > 0 {
			return r.usd / r.btc, "", true
		}
	case "fee":
		return r.fee, "", r.fee != 0
	case "fee_btc":
		return r.feeBTC, "", r.feeBTC != 0
	case "gain":
		return r.gain, "", r.hasGain
	case "days_held":
		return r.days, "", r.days > 0
	case "weekday":
		return 0, weekdays[r.t.Weekday()], true
	case "month":
		return 0, r.t.Month().String(), true
	case "year":
		return 0, strconv.Itoa(r.t.Year()), true
	case "account":
		return 0, r.acct, r.acct != ""
	case "kind":
		return 0, kindName(r.kind), true
	}
	return 0, "", false
}

func groupKey(r statRow, by string) string {
	switch by {
	case "year":
		return strconv.Itoa(r.t.Year())
	case "month":
		return r.t.Format("2006-01")
	case "weekday":
		return weekdays[r.t.Weekday()]
	case "account":
		return r.acct
	case "kind":
		return kindName(r.kind)
	}
	return ""
}

// fmtVal writes a value in the measure's unit.
func fmtVal(m string, v float64) string {
	switch m {
	case "", "usd", "price", "fee", "gain":
		return usdStr(int64(math.Round(v * 100)))
	case "btc", "fee_btc":
		return btcStr(int64(math.Round(v * 1e8)))
	case "days_held":
		return fmt.Sprintf("%.0f days", v)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// niceStep picks a round bucket size for a set of values.
func niceStep(vals []float64, buckets float64) float64 {
	lo, hi := vals[0], vals[len(vals)-1]
	span := hi - lo
	if span <= 0 {
		span = math.Abs(hi)
	}
	if span <= 0 {
		return 1
	}
	raw := span / buckets
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, k := range []float64{1, 2, 2.5, 5, 10} {
		if k*p >= raw {
			return k * p
		}
	}
	return 10 * p
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	i := int(pos)
	if i >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[i] + (sorted[i+1]-sorted[i])*(pos-float64(i))
}

func (tb *toolbox) analyze(a map[string]any) (string, string, error) {
	src := strings.ToLower(argStr(a, "source"))
	m := strings.ToLower(argStr(a, "measure"))
	stat := strings.ToLower(argStr(a, "stat"))
	by := strings.ToLower(argStr(a, "group_by"))
	if by == "none" {
		by = ""
	}
	if stat == "" {
		stat = "summary"
	}
	if stat == "average" || stat == "avg" {
		stat = "mean"
	}
	if stat == "most_common" || stat == "most common" {
		stat = "mode"
	}
	rows, src, err := tb.statRows(src)
	if err != nil {
		return "", "", err
	}
	from, hasFrom := parseDay(argStr(a, "from"))
	to, hasTo := parseDay(argStr(a, "to"))
	acct := argStr(a, "account")
	if acct != "" {
		if n, err := tb.resolveAccount(acct); err == nil {
			acct = n
		}
	}
	kind := argStr(a, "kind")
	_, hasMin := a["min"]
	_, hasMax := a["max"]
	lo, hi := argNum(a, "min"), argNum(a, "max")
	categorical := map[string]bool{"weekday": true, "month": true, "year": true, "account": true, "kind": true}[m]
	if categorical && stat != "count" && stat != "mode" && stat != "summary" && stat != "histogram" {
		return "", "", fmt.Errorf("%s is a category: use stat count or mode", m)
	}
	type item struct {
		r   statRow
		v   float64
		cat string
	}
	groups := map[string][]item{}
	var all []item
	for _, r := range rows {
		if hasFrom && r.t.Before(from) || hasTo && r.t.After(to.Add(24*time.Hour-time.Second)) {
			continue
		}
		if acct != "" && !strings.EqualFold(r.acct, acct) {
			continue
		}
		if kind != "" && !kindMatches(kind, r.kind) {
			continue
		}
		v, cat, ok := statValue(r, m)
		if !ok {
			continue
		}
		if !categorical && (hasMin && v < lo || hasMax && v > hi) {
			continue
		}
		it := item{r, v, cat}
		all = append(all, it)
		g := groupKey(r, by)
		groups[g] = append(groups[g], it)
	}
	unit := map[string]string{"": "USD amount", "usd": "USD amount", "btc": "BTC amount", "price": "price per BTC", "fee": "fee (USD)", "fee_btc": "fee (BTC)",
		"gain": "gain", "days_held": "days held"}[m]
	if unit == "" {
		unit = m
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s of %s, %d rows", strings.ToUpper(stat[:1])+stat[1:], unit, len(all))
	if src != "" {
		fmt.Fprintf(&b, " (source %s", src)
		if acct != "" {
			fmt.Fprintf(&b, ", account %s", acct)
		}
		if hasFrom || hasTo {
			fmt.Fprintf(&b, ", %s to %s", argStr(a, "from"), argStr(a, "to"))
		}
		b.WriteString(")")
	}
	b.WriteString(":\n")
	if len(all) == 0 {
		b.WriteString("No rows match.\n")
		return b.String(), "Analyzed " + src, nil
	}
	step := argNum(a, "round_to")
	topN := int(argNum(a, "top_n"))
	if topN <= 0 {
		topN = 10
	}
	topN = min(topN, 40)

	describe := func(items []item, indent string) {
		if categorical {
			counts := map[string]int{}
			for _, it := range items {
				counts[it.cat]++
			}
			keys := make([]string, 0, len(counts))
			for k := range counts {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if counts[keys[i]] != counts[keys[j]] {
					return counts[keys[i]] > counts[keys[j]]
				}
				return keys[i] < keys[j]
			})
			for i, k := range keys {
				if i >= 20 {
					break
				}
				fmt.Fprintf(&b, "%s- %s: %d (%.0f%%)\n", indent, k, counts[k], 100*float64(counts[k])/float64(len(items)))
			}
			return
		}
		vals := make([]float64, len(items))
		var sum float64
		for i, it := range items {
			vals[i] = it.v
			sum += it.v
		}
		sort.Float64s(vals)
		n := float64(len(vals))
		mean := sum / n
		line := func(label, v string) { fmt.Fprintf(&b, "%s%s: %s\n", indent, label, v) }
		// For prices, the meaningful average is total spent / total BTC.
		if m == "price" && (stat == "mean" || stat == "summary") {
			var u, c float64
			for _, it := range items {
				u += it.r.usd
				c += it.r.btc
			}
			if c > 0 {
				defer line("average weighted by amount (total USD / total BTC; use this as the average price paid)", fmtVal(m, u/c))
			}
		}
		switch stat {
		case "summary":
			fmt.Fprintf(&b, "%scount %d, sum %s, mean %s, median %s, min %s, max %s\n", indent, len(vals), fmtVal(m, sum), fmtVal(m, mean), fmtVal(m, percentile(vals, .5)), fmtVal(m, vals[0]), fmtVal(m, vals[len(vals)-1]))
		case "count":
			line("count", strconv.Itoa(len(vals)))
		case "sum":
			line("sum", fmtVal(m, sum))
		case "mean":
			line("mean", fmtVal(m, mean))
		case "median":
			line("median", fmtVal(m, percentile(vals, .5)))
		case "min":
			line("min", fmtVal(m, vals[0]))
		case "max":
			line("max", fmtVal(m, vals[len(vals)-1]))
		case "stddev":
			var ss float64
			for _, v := range vals {
				ss += (v - mean) * (v - mean)
			}
			line("standard deviation", fmtVal(m, math.Sqrt(ss/n)))
		case "percentiles":
			fmt.Fprintf(&b, "%s10th %s, 25th %s, median %s, 75th %s, 90th %s\n", indent, fmtVal(m, percentile(vals, .1)), fmtVal(m, percentile(vals, .25)), fmtVal(m, percentile(vals, .5)), fmtVal(m, percentile(vals, .75)), fmtVal(m, percentile(vals, .9)))
		case "mode":
			s, auto := step, false
			counts := map[float64]int{}
			count := func() {
				counts = map[float64]int{}
				for _, v := range vals {
					k := v
					if s > 0 {
						k = math.Round(v/s) * s
					} else {
						k = math.Round(v*100) / 100
					}
					counts[k]++
				}
			}
			count()
			best := 0
			for _, c := range counts {
				best = max(best, c)
			}
			if best == 1 && s == 0 && len(vals) > 1 { // no exact repeats: bucket
				s, auto = niceStep(vals, 12), true
				count()
			}
			keys := make([]float64, 0, len(counts))
			for k := range counts {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if counts[keys[i]] != counts[keys[j]] {
					return counts[keys[i]] > counts[keys[j]]
				}
				return keys[i] < keys[j]
			})
			if auto {
				fmt.Fprintf(&b, "%s(no exact value repeats, so values were rounded to the nearest %s)\n", indent, fmtVal(m, s))
			} else if s > 0 {
				fmt.Fprintf(&b, "%s(values rounded to the nearest %s)\n", indent, fmtVal(m, s))
			}
			for i, k := range keys {
				if i >= 5 {
					break
				}
				label := "next"
				if i == 0 {
					label = "most common"
				}
				fmt.Fprintf(&b, "%s%s: %s, %d of %d rows (%.0f%%)\n", indent, label, fmtVal(m, k), counts[k], len(vals), 100*float64(counts[k])/n)
			}
		case "histogram":
			s := step
			if s <= 0 {
				s = niceStep(vals, 8)
			}
			bins := map[float64]int{}
			for _, v := range vals {
				bins[math.Floor(v/s)*s]++
			}
			keys := make([]float64, 0, len(bins))
			for k := range bins {
				keys = append(keys, k)
			}
			sort.Float64s(keys)
			for i, k := range keys {
				if i >= 30 {
					fmt.Fprintf(&b, "%s(%d more bins)\n", indent, len(keys)-30)
					break
				}
				fmt.Fprintf(&b, "%s- %s to %s: %d\n", indent, fmtVal(m, k), fmtVal(m, k+s), bins[k])
			}
		case "top", "bottom":
			sorted := append([]item(nil), items...)
			sort.Slice(sorted, func(i, j int) bool {
				if stat == "top" {
					return sorted[i].v > sorted[j].v
				}
				return sorted[i].v < sorted[j].v
			})
			for i, it := range sorted {
				if i >= topN {
					break
				}
				fmt.Fprintf(&b, "%s- %s: [%s] %s %s, %s", indent, fmtVal(m, it.v), it.r.key, dayOf(it.r.t), it.r.acct, btcStr(int64(math.Round(it.r.btc*1e8))))
				if it.r.hasUSD && m != "usd" && m != "" {
					fmt.Fprintf(&b, ", %s", usdStr(int64(math.Round(it.r.usd*100))))
				}
				b.WriteString("\n")
			}
		default:
			fmt.Fprintf(&b, "%sunknown stat %q\n", indent, stat)
		}
	}
	if stat != "summary" && stat != "count" && stat != "sum" && stat != "mean" && stat != "median" && stat != "min" && stat != "max" &&
		stat != "stddev" && stat != "percentiles" && stat != "mode" && stat != "histogram" && stat != "top" && stat != "bottom" {
		return "", "", fmt.Errorf("unknown stat %q", stat)
	}
	if by == "" {
		describe(all, "")
	} else {
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		if by == "weekday" {
			order := map[string]int{}
			for i, d := range weekdays {
				order[d] = i
			}
			sort.Slice(keys, func(i, j int) bool { return order[keys[i]] < order[keys[j]] })
		} else if by == "account" || by == "kind" {
			sort.Slice(keys, func(i, j int) bool { return len(groups[keys[i]]) > len(groups[keys[j]]) })
		} else {
			sort.Strings(keys)
		}
		for i, k := range keys {
			if i >= 40 {
				fmt.Fprintf(&b, "(%d more groups)\n", len(keys)-40)
				break
			}
			fmt.Fprintf(&b, "%s (%d rows):\n", k, len(groups[k]))
			describe(groups[k], "  ")
		}
		if stat != "top" && stat != "bottom" {
			fmt.Fprintf(&b, "ALL (%d rows):\n", len(all))
			describe(all, "  ")
		}
	}
	return b.String(), fmt.Sprintf("Analyzed %s (%s of %s)", src, stat, unit), nil
}
