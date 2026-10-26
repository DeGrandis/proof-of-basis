package app

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
)

// analyze_records: one tool to list, total and describe the user's
// records. Searching, totals, statistics, lot timing, losses, Review
// items, income and Lightning activity all go through it; the server does
// the math and the model only picks what to look at. It can also hand the
// rows over as a CSV download or draw a chart.

func analyzeToolSpec() ollama.Tool {
	return ollama.Tool{Type: "function", Function: ollama.ToolSpec{
		Name: "analyze_records",
		Description: "List, count, total or describe records with exact math. Pick the records (source and filters), what to measure, and the statistic; optionally group, export or chart. " +
			"Use it for searches, counts, totals, averages, medians, most common values, largest/smallest, distributions and per-year/per-account questions. Examples: " +
			"most common buy amount = source buys, measure usd, stat mode; average price paid per year = source buys, measure price, stat mean, group_by year; " +
			"biggest buy = source buys, stat top, top_n 1 (smallest: stat bottom); oldest coins still held = source lots, measure days_held, stat top; " +
			"which weekday I buy on = measure weekday, stat count;usual holding time = source sales, measure days_held, stat median; " +
			"lots turning long-term in 60 days = source lots, measure days_to_long_term, max 60, stat list; lots at a loss = source lots, measure gain, max 0, stat list; " +
			"find a record = stat list with filters (from, to, account, kind, text).",
		Parameters: obj(map[string]any{
			"source":   str("buys (default), sales (one row per sale), records (every record), income, review (items waiting for a label), lots (coins still held), lightning (node activity)"),
			"measure":  str("usd (default), btc, price (USD per BTC), fee, fee_btc, gain, days_held, days_to_long_term (lots), or a category: weekday, month, year, account, kind"),
			"stat":     str("summary (default), list (the records themselves), count, sum, mean, median, mode (most common), min, max, stddev, percentiles, histogram, top, bottom"),
			"group_by": str("none, year, month, weekday, account or kind"),
			"from":     str("first day YYYY-MM-DD"),
			"to":       str("last day YYYY-MM-DD"),
			"account":  str("account name"),
			"kind":     str("kind: buy, sell, transfer, income, spend, unmatched…; for review: in or out"),
			"text":     str("text in the note, label, txid, address or id"),
			"min":      num("filter: keep only records whose measure is at least this value (not a count)"),
			"max":      num("filter: keep only records whose measure is at most this value (not a count; for the N largest use stat top with top_n)"),
			"round_to": num("bucket size for mode/histogram, e.g. 10 for $10"),
			"top_n":    num("rows for list/top/bottom, default 10 (list: 40)"),
			"sort":     str("for list: newest (default), oldest, largest, smallest"),
			"export":   map[string]any{"type": "boolean", "description": "also give the user a CSV download of every matching row"},
			"chart":    map[string]any{"type": "boolean", "description": "also draw a chart (with group_by or histogram)"},
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
	toLong          float64 // lots: days until long-term (0 if already)
	text            string  // searchable: note, label, txid, address
	line            string  // how the record reads in a list
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
				usd: float64(g.Cents) / 100, btc: float64(g.Sats) / 1e8, hasUSD: g.Cents != 0, fee: float64(g.FeeCents) / 100, feeBTC: float64(g.FeeSats) / 1e8,
				text: strings.ToLower(g.Note + " " + g.Label + " " + g.Txid + " " + g.Address + " " + g.Key + " " + g.To), line: graphLine(g)})
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
				rows = append(rows, statRow{key: d.OpID, acct: d.Account, kind: "sale", t: d.Disposed.UTC(), hasUSD: true, hasGain: true, text: strings.ToLower(d.OpID)})
			}
			r := &rows[i]
			btc := float64(d.Sats) / 1e8
			r.days = (r.days*r.btc + d.Disposed.Sub(d.Acquired).Hours()/24*btc) / math.Max(r.btc+btc, 1e-12)
			r.btc += btc
			r.usd += float64(d.Proceeds) / 100
			r.gain += float64(d.Gain()) / 100
		}
		for i := range rows {
			r := &rows[i]
			r.line = fmt.Sprintf("[%s] %s %s sold %s for %s, gain %s, held %.0f days on average", r.key, dayOf(r.t), r.acct, btcStr(int64(math.Round(r.btc*1e8))), usdStr(int64(math.Round(r.usd*100))), usdStr(int64(math.Round(r.gain*100))), r.days)
		}
	case "income":
		for _, x := range tb.rep.Income {
			rows = append(rows, statRow{key: x.Key, kind: x.Kind, t: x.Time.UTC(), usd: float64(x.Cents) / 100, btc: float64(x.Sats) / 1e8, hasUSD: true, text: strings.ToLower(x.Key + " " + x.Kind),
				line: fmt.Sprintf("[%s] %s %s: %s, worth %s when received", x.Key, dayOf(x.Time), x.Kind, btcStr(x.Sats), usdStr(x.Cents))})
		}
	case "review":
		for _, it := range tb.rep.Unmatched {
			rows = append(rows, statRow{key: it.Key, acct: it.Account, kind: string(it.Dir), t: it.Time.UTC(), usd: float64(it.Value) / 100, btc: float64(it.Sats) / 1e8, hasUSD: it.Value != 0, feeBTC: float64(it.FeeSats) / 1e8,
				text: strings.ToLower(it.Note + " " + it.Txid + " " + it.Address + " " + it.Key + " " + it.Network), line: itemLine(it.Key, it.Time, it.Account, string(it.Dir), it.Sats, it.FeeSats, it.Value, it.Network, it.Note, it.Txid)})
		}
	case "lots":
		now := time.Now()
		for _, l := range tb.rep.OpenLots {
			if l.Sats <= 0 {
				continue
			}
			worth := float64(l.Sats) / 1e8 * float64(tb.rep.Price) / 100
			r := statRow{key: l.ID, acct: l.Account, kind: "lot", t: l.Acquired.UTC(), usd: float64(l.Basis) / 100, btc: float64(l.Sats) / 1e8, hasUSD: true,
				days: now.Sub(l.Acquired).Hours() / 24, gain: worth - float64(l.Basis)/100, hasGain: tb.rep.Price > 0, text: strings.ToLower(l.ID + " " + l.OriginID)}
			if longOn := l.Acquired.AddDate(1, 0, 1); longOn.After(now) {
				r.toLong = math.Ceil(longOn.Sub(now).Hours() / 24)
			}
			term := "long-term"
			if r.toLong > 0 {
				term = fmt.Sprintf("long-term on %s (in %.0f days)", dayOf(l.Acquired.AddDate(1, 0, 1)), r.toLong)
			}
			r.line = fmt.Sprintf("lot %s in %s: %s acquired %s, basis %s (%s per BTC), unrealized gain %s, %s, from [%s]", l.ID, l.Account, btcStr(l.Sats), dayOf(l.Acquired), usdStr(l.Basis),
				usdStr(int64(math.Round(r.usd/r.btc*100))), usdStr(int64(math.Round(r.gain*100))), term, l.OriginID)
			rows = append(rows, r)
		}
	case "lightning":
		if ln := tb.rep.Lightning; ln != nil {
			for _, e := range ln.Events {
				net := e.CreditSats - e.DebitSats
				rows = append(rows, statRow{key: e.UID, acct: ln.Account, kind: e.Tag, t: time.Unix(e.Time, 0).UTC(), btc: float64(net) / 1e8, text: strings.ToLower(e.Description + " " + e.Tag + " " + e.Txid),
					line: fmt.Sprintf("%s %s: in %s, out %s %s", dayOf(time.Unix(e.Time, 0)), e.Tag, btcStr(e.CreditSats), btcStr(e.DebitSats), clip(e.Description, 60))})
			}
		}
	default:
		return nil, "", errors.New("source must be buys, sales, records, income, review, lots or lightning")
	}
	return rows, src, nil
}

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
	case "days_to_long_term":
		return r.toLong, "", r.toLong > 0
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

func fmtVal(m string, v float64) string {
	switch m {
	case "", "usd", "price", "fee", "gain":
		return usdStr(int64(math.Round(v * 100)))
	case "btc", "fee_btc":
		return btcStr(int64(math.Round(v * 1e8)))
	case "days_held", "days_to_long_term":
		return fmt.Sprintf("%.0f days", v)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

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

// Chart is a small bar chart drawn under an answer.
type Chart struct {
	Title  string    `json:"title"`
	Labels []string  `json:"labels"`
	Values []float64 `json:"values"`
	Unit   string    `json:"unit"` // usd, btc, days or count
}

var validStats = map[string]bool{"summary": true, "list": true, "count": true, "sum": true, "mean": true, "median": true, "mode": true, "min": true, "max": true,
	"stddev": true, "percentiles": true, "histogram": true, "top": true, "bottom": true}

var validMeasures = map[string]bool{"": true, "usd": true, "btc": true, "price": true, "fee": true, "fee_btc": true, "gain": true, "days_held": true,
	"days_to_long_term": true, "weekday": true, "month": true, "year": true, "account": true, "kind": true}

// normalizeAnalyzeArgs accepts the label tool's wording too, which small
// models mix up: a nested match, min_usd/max_usd, min_btc/max_btc,
// direction and network.
func normalizeAnalyzeArgs(a map[string]any) {
	if m, ok := a["match"].(map[string]any); ok {
		for k, v := range m {
			if _, set := a[k]; !set {
				a[k] = v
			}
		}
		delete(a, "match")
	}
	for _, unit := range []string{"usd", "btc"} {
		for _, side := range []string{"min", "max"} {
			if v, ok := a[side+"_"+unit]; ok {
				if _, set := a[side]; !set {
					a[side] = v
					if argStr(a, "measure") == "" {
						a["measure"] = unit
					}
				}
				delete(a, side+"_"+unit)
			}
		}
	}
	if d := argStr(a, "direction"); d != "" && argStr(a, "kind") == "" {
		a["kind"] = d
	}
	if n := argStr(a, "network"); n != "" && argStr(a, "text") == "" {
		a["text"] = n
	}
}

func (tb *toolbox) analyze(a map[string]any, lk *Lookup) (string, string, error) {
	normalizeAnalyzeArgs(a)
	src := strings.ToLower(argStr(a, "source"))
	m := strings.ToLower(argStr(a, "measure"))
	stat := strings.ToLower(argStr(a, "stat"))
	by := strings.ToLower(argStr(a, "group_by"))
	if by == "none" {
		by = ""
	}
	switch stat {
	case "":
		stat = "summary"
	case "average", "avg":
		stat = "mean"
	case "most_common", "most common":
		stat = "mode"
	case "search", "rows", "records":
		stat = "list"
	}
	if !validStats[stat] {
		return "", "", fmt.Errorf("unknown stat %q", stat)
	}
	switch m {
	case "fee_usd", "usd_fee", "fees", "fee_usd_total":
		m = "fee"
	case "fee_sats", "fees_btc", "btc_fee":
		m = "fee_btc"
	case "amount", "value", "usd_value", "amount_usd", "cost", "proceeds":
		m = "usd"
	case "amount_btc", "btc_amount", "sats", "quantity":
		m = "btc"
	}
	if !validMeasures[m] {
		// An unknown measure would match nothing and read as zero.
		return "", "", fmt.Errorf("unknown measure %q; use one of usd, btc, price, fee, fee_btc, gain, days_held, days_to_long_term, weekday, month, year, account, kind", m)
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
	text := strings.ToLower(argStr(a, "text"))
	_, hasMin := a["min"]
	_, hasMax := a["max"]
	lo, hi := argNum(a, "min"), argNum(a, "max")
	categorical := map[string]bool{"weekday": true, "month": true, "year": true, "account": true, "kind": true}[m]
	if categorical && stat != "count" && stat != "mode" && stat != "summary" && stat != "histogram" && stat != "list" {
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
		if kind != "" && !kindMatches(kind, r.kind) && !strings.EqualFold(kind, r.kind) && !(src == "review" && reviewDir(kind) == r.kind) {
			continue
		}
		if text != "" && !strings.Contains(r.text, text) {
			continue
		}
		v, cat, ok := statValue(r, m)
		if !ok {
			if stat != "list" || m != "" || hasMin || hasMax {
				continue
			}
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
		"gain": "gain", "days_held": "days held", "days_to_long_term": "days until long-term"}[m]
	if unit == "" {
		unit = m
	}
	var b strings.Builder
	if stat == "list" {
		fmt.Fprintf(&b, "%d %s records match", len(all), src)
	} else {
		fmt.Fprintf(&b, "%s of %s, %d rows (source %s", strings.ToUpper(stat[:1])+stat[1:], unit, len(all), src)
		if acct != "" {
			fmt.Fprintf(&b, ", account %s", acct)
		}
		if hasFrom || hasTo {
			fmt.Fprintf(&b, ", %s to %s", argStr(a, "from"), argStr(a, "to"))
		}
		b.WriteString(")")
	}
	switch {
	case hasMin && hasMax:
		fmt.Fprintf(&b, " [only values %g to %g]", lo, hi)
	case hasMin:
		fmt.Fprintf(&b, " [only values at least %g]", lo)
	case hasMax:
		fmt.Fprintf(&b, " [only values at most %g]", hi)
	}
	b.WriteString(":\n")
	summary := fmt.Sprintf("Analyzed %s (%s of %s)", src, stat, unit)
	if stat == "list" {
		summary = fmt.Sprintf("Found %d %s records", len(all), src)
	}
	if len(all) == 0 {
		b.WriteString("No rows match.\n")
		if hasMin || hasMax {
			// Models often send "max 1" meaning "just one": say what it did.
			b.WriteString("min and max filter by value; they don't limit how many rows. For the largest or smallest N, leave them out and use stat top or bottom with top_n.\n")
		}
		return b.String(), summary, nil
	}
	if want, _ := a["export"].(bool); want {
		name, csvData := exportCSV(src, m, func(yield func(statRow, float64)) {
			for _, it := range all {
				yield(it.r, it.v)
			}
		})
		// Name the file after what's in it.
		parts := []string{"proof-of-basis", src}
		if acct != "" {
			parts = append(parts, acct)
		}
		if kind != "" {
			parts = append(parts, kind)
		}
		if hasFrom || hasTo {
			f, t := argStr(a, "from"), argStr(a, "to")
			if f == "" {
				f = "start"
			}
			if t == "" {
				t = time.Now().Format("2006-01-02")
			}
			parts = append(parts, f+"-to-"+t)
		}
		name = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			if r == ' ' {
				return '-'
			}
			return -1
		}, strings.Join(parts, "-")) + ".csv"
		tok := tb.s.addExport(name, csvData)
		lk.File = &FileRef{Name: name, URL: "/api/ai/export/" + tok, Rows: len(all)}
		summary = fmt.Sprintf("Made %s", name)
		fmt.Fprintf(&b, "A CSV file of all %d rows (%s) is shown to the user as a download under your answer.\n", len(all), name)
	}
	step := argNum(a, "round_to")
	topN := int(argNum(a, "top_n"))
	if topN <= 0 {
		topN = 10
		if stat == "list" {
			topN = 40
		}
	}
	topN = min(topN, 60)
	chartWanted, _ := a["chart"].(bool)
	var chart *Chart
	chartPoint := func(label string, v float64) {
		if chart != nil {
			chart.Labels = append(chart.Labels, label)
			chart.Values = append(chart.Values, v)
		}
	}
	if chartWanted {
		u := map[string]string{"": "usd", "usd": "usd", "price": "usd", "fee": "usd", "gain": "usd", "btc": "btc", "fee_btc": "btc", "days_held": "days", "days_to_long_term": "days"}[m]
		if categorical || stat == "count" {
			u = "count"
		}
		chart = &Chart{Title: strings.ToUpper(stat[:1]) + stat[1:] + " of " + unit, Unit: u}
	}

	// one number per group, for charts
	single := func(items []item) (float64, bool) {
		vals := make([]float64, 0, len(items))
		var sum float64
		for _, it := range items {
			vals = append(vals, it.v)
			sum += it.v
		}
		sort.Float64s(vals)
		switch stat {
		case "count":
			return float64(len(vals)), true
		case "sum", "summary":
			return sum, true
		case "mean":
			if m == "price" {
				var u, c float64
				for _, it := range items {
					u += it.r.usd
					c += it.r.btc
				}
				if c > 0 {
					return u / c, true
				}
			}
			return sum / float64(len(vals)), true
		case "median":
			return percentile(vals, .5), true
		case "min":
			return vals[0], true
		case "max":
			return vals[len(vals)-1], true
		}
		return 0, false
	}

	var shown []statRow // rows a list printed, for the Label control
	describe := func(items []item, indent string) {
		if stat == "list" {
			sorted := append([]item(nil), items...)
			switch strings.ToLower(argStr(a, "sort")) {
			case "oldest":
				sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].r.t.Before(sorted[j].r.t) })
			case "largest":
				sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })
			case "smallest":
				sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].v < sorted[j].v })
			default:
				if m == "days_to_long_term" {
					sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].v < sorted[j].v })
				} else {
					sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].r.t.After(sorted[j].r.t) })
				}
			}
			var tsats, tusd, tgain float64
			for _, it := range items {
				tsats += it.r.btc
				tusd += it.r.usd
				tgain += it.r.gain
			}
			fmt.Fprintf(&b, "%stotal %s", indent, btcStr(int64(math.Round(tsats*1e8))))
			if items[0].r.hasUSD {
				fmt.Fprintf(&b, ", %s", usdStr(int64(math.Round(tusd*100))))
			}
			if items[0].r.hasGain {
				fmt.Fprintf(&b, ", gain %s", usdStr(int64(math.Round(tgain*100))))
			}
			b.WriteString("\n")
			for i, it := range sorted {
				if i >= topN {
					fmt.Fprintf(&b, "%s(%d more; narrow the filters, or use export)\n", indent, len(sorted)-topN)
					break
				}
				fmt.Fprintf(&b, "%s- %s\n", indent, it.r.line)
				shown = append(shown, it.r)
			}
			return
		}
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
			if m == "weekday" && chart != nil {
				for _, d := range weekdays {
					chartPoint(d[:3], float64(counts[d]))
				}
			}
			for i, k := range keys {
				if i >= 20 {
					break
				}
				fmt.Fprintf(&b, "%s- %s: %d (%.0f%%)\n", indent, k, counts[k], 100*float64(counts[k])/float64(len(items)))
				if m != "weekday" {
					chartPoint(k, float64(counts[k]))
				}
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
					k := math.Round(v*100) / 100
					if s > 0 {
						k = math.Round(v/s) * s
					}
					counts[k]++
				}
			}
			count()
			best := 0
			for _, c := range counts {
				best = max(best, c)
			}
			if best == 1 && s == 0 && len(vals) > 1 {
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
				if indent == "" {
					chartPoint(fmtVal(m, k), float64(bins[k]))
				}
			}
			if chart != nil {
				chart.Unit = "count"
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
				fmt.Fprintf(&b, "%s- %s: %s\n", indent, fmtVal(m, it.v), it.r.line)
			}
		}
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
			if v, ok := single(groups[k]); ok {
				chartPoint(k, v)
			}
		}
		if stat != "top" && stat != "bottom" && stat != "list" {
			fmt.Fprintf(&b, "ALL (%d rows):\n", len(all))
			describe(all, "  ")
		}
	}
	if src == "review" {
		rows := shown
		if stat != "list" {
			rows = nil
			for _, it := range all {
				rows = append(rows, it.r)
			}
		}
		if len(rows) > 0 && len(rows) <= 500 {
			rs := &ReviewSet{}
			for _, r := range rows {
				rs.Keys = append(rs.Keys, r.key)
				switch {
				case rs.Dir == "":
					rs.Dir = r.kind
				case rs.Dir != r.kind:
					rs.Dir = "mixed"
				}
			}
			lk.Review = rs
		}
	}
	if chart != nil {
		if len(chart.Labels) >= 2 {
			if len(chart.Labels) > 40 {
				chart.Labels, chart.Values = chart.Labels[len(chart.Labels)-40:], chart.Values[len(chart.Values)-40:]
			}
			lk.Chart = chart
			b.WriteString("A chart of this is shown to the user under your answer.\n")
		} else {
			b.WriteString("(No chart: charts need group_by, a histogram or a category.)\n")
		}
	}
	return b.String(), summary, nil
}

func exportCSV(src, m string, each func(func(statRow, float64))) (string, []byte) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"date", "account", "kind", "btc", "usd", "price_per_btc", "fee_usd", "fee_btc", "gain_usd", "days_held", "id"})
	each(func(r statRow, _ float64) {
		price := ""
		if r.hasUSD && r.btc > 0 {
			price = strconv.FormatFloat(math.Round(r.usd/r.btc*100)/100, 'f', 2, 64)
		}
		f := func(v float64, d int) string {
			if v == 0 {
				return ""
			}
			return strconv.FormatFloat(v, 'f', d, 64)
		}
		w.Write([]string{dayOf(r.t), r.acct, r.kind, strconv.FormatFloat(r.btc, 'f', 8, 64), f(r.usd, 2), price, f(r.fee, 2), f(r.feeBTC, 8), f(r.gain, 2), f(math.Round(r.days), 0), r.key})
	})
	w.Flush()
	return "proof-of-basis-" + src + "-" + time.Now().Format("2006-01-02") + ".csv", buf.Bytes()
}

// reviewDir maps the words people use to a Review item's direction:
// spends, disposals, sales and sends are outgoing; income and deposits in.
func reviewDir(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	for _, w := range []string{"out", "outflow", "outgoing", "send", "sent", "spend", "spent", "disposal", "dispose", "sell", "sold", "sale", "payment", "withdrawal", "gift", "donation"} {
		if strings.HasPrefix(k, w) {
			return "out"
		}
	}
	for _, w := range []string{"in", "inflow", "incoming", "receive", "received", "deposit", "income", "buy", "purchase", "refund"} {
		if strings.HasPrefix(k, w) {
			return "in"
		}
	}
	return ""
}
