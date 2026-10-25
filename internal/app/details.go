package app

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"btctax/internal/lots"
	"btctax/internal/tax"
	"btctax/internal/trace"
)

// DigitalAssetQuestion is the year's answer to the yes/no question on
// Form 1040, with why. Gifts and donations are listed as a question for
// the tax professional rather than decided here.
type DigitalAssetQuestion struct {
	Answer  string   `json:"answer"` // "Yes" or "No"
	Reasons []string `json:"reasons"`
	Ask     []string `json:"ask,omitempty"`
}

// GiftRow is BTC given away, donated, lost, or received as a gift, with
// what a tax professional needs to report it.
type GiftRow struct {
	Key       string   `json:"key"`
	Time      int64    `json:"t"`
	Account   string   `json:"account"`
	Category  string   `json:"category"` // gift, donation, lost, gift-in
	Sats      int64    `json:"sats"`
	Value     int64    `json:"value"` // market value that day
	Basis     int64    `json:"basis"`
	Acquired  string   `json:"acquired,omitempty"`
	Term      string   `json:"term,omitempty"` // long, short, mixed
	Deduction int64    `json:"deduction,omitempty"`
	Recipient string   `json:"recipient,omitempty"`
	Note      string   `json:"note,omitempty"`
	Flags     []string `json:"flags"`
}

// annualExclusion is the gift tax annual exclusion per recipient, by year,
// as the IRS publishes it. Later years use the last known amount.
var annualExclusion = map[int]int64{2017: 14000_00, 2018: 15000_00, 2019: 15000_00, 2020: 15000_00, 2021: 15000_00,
	2022: 16000_00, 2023: 17000_00, 2024: 18000_00, 2025: 19000_00, 2026: 19000_00}

func exclusionFor(year int) int64 {
	if v, ok := annualExclusion[year]; ok {
		return v
	}
	last := 0
	for y := range annualExclusion {
		if y <= year && y > last {
			last = y
		}
	}
	if last == 0 {
		return annualExclusion[2017]
	}
	return annualExclusion[last]
}

// documentYears adds to each year its income by type and the 1040 answer,
// and builds the gifts and donations list.
func documentYears(r *Report, p *pipeline, res *lots.Result) {
	z := loc(p.set)
	byYear := map[int]*YearSummary{}
	for i := range r.Years {
		byYear[r.Years[i].Year] = &r.Years[i]
	}
	year := func(t time.Time) *YearSummary { return byYear[t.In(z).Year()] }
	for _, in := range p.tr.Income {
		if y := year(in.Time); y != nil {
			if y.IncomeByType == nil {
				y.IncomeByType = map[string]int64{}
			}
			y.IncomeByType[in.Type] += in.Cents
		}
	}
	// Why the 1040 answer is yes, counted per year.
	type why struct{ sales, fees, income, given int }
	counts := map[int]*why{}
	c := func(t time.Time) *why {
		yr := t.In(z).Year()
		if counts[yr] == nil {
			counts[yr] = &why{}
		}
		return counts[yr]
	}
	opKind := map[string]lots.Op{}
	for _, o := range p.tr.Ops {
		opKind[o.ID] = o
		if o.Kind == lots.Transfer && o.FeeSats > 0 {
			c(o.Time).fees++
		}
	}
	seen := map[string]bool{}
	for _, d := range res.Disposals {
		if seen[d.OpID] {
			continue
		}
		seen[d.OpID] = true
		if opKind[d.OpID].Kind == lots.Dispose {
			c(d.Disposed).sales++
		}
	}
	for _, in := range p.tr.Income {
		c(in.Time).income++
	}
	// Gifts, donations and losses.
	r.Gifts = []GiftRow{}
	rules := p.cfg.Rules
	if rules == nil {
		rules = tax.US{}
	}
	groups := map[string]*GiftRow{}
	var order []string
	first := map[string]time.Time{}
	terms := map[string]map[bool]bool{}
	for _, rm := range res.Removals {
		g := groups[rm.OpID]
		if g == nil {
			l := p.labels[rm.OpID]
			g = &GiftRow{Key: rm.OpID, Time: rm.Removed.Unix(), Account: rm.Account, Category: string(l.Category), Recipient: l.Recipient, Note: l.Note, Flags: []string{}}
			groups[rm.OpID], terms[rm.OpID] = g, map[bool]bool{}
			order = append(order, rm.OpID)
			c(rm.Removed).given++
		}
		g.Sats += rm.Sats
		g.Basis += rm.Basis
		if f, ok := first[rm.OpID]; !ok || rm.Acquired.Before(f) {
			first[rm.OpID] = rm.Acquired
		}
		terms[rm.OpID][rm.LotID != "" && rules.LongTerm(rm.Acquired, rm.Removed)] = true
	}
	given := map[[2]string]int64{} // year|recipient -> value given
	for _, k := range order {
		g := groups[k]
		t := time.Unix(g.Time, 0).In(z)
		if p, ok := p.prices.lookup(t); ok {
			g.Value = lots.MulDiv(p, g.Sats, 100_000_000)
		}
		g.Acquired = first[k].In(z).Format("2006-01-02")
		switch {
		case terms[k][true] && terms[k][false]:
			g.Term = "mixed"
		case terms[k][true]:
			g.Term = "long"
		default:
			g.Term = "short"
		}
		switch g.Category {
		case string(trace.Donation):
			// Held over a year: market value; otherwise the lower of basis
			// and value (IRS FAQ 35).
			g.Deduction = g.Value
			if g.Term != "long" {
				g.Deduction = min(g.Basis, g.Value)
				g.Flags = append(g.Flags, "Some coins were held a year or less: the deduction is usually the lower of basis and market value for those.")
			}
			if g.Value >= 250_00 {
				g.Flags = append(g.Flags, "Keep the charity's written acknowledgment (needed at $250 or more).")
			}
			if g.Value > 5000_00 {
				g.Flags = append(g.Flags, "Over $5,000: Form 8283 Section B, and ask about a qualified appraisal.")
			}
			if g.Recipient == "" {
				g.Flags = append(g.Flags, "Add the charity's name.")
			}
		case string(trace.Gift):
			given[[2]string{fmt.Sprint(t.Year()), g.Recipient}] += g.Value
			if g.Recipient == "" {
				g.Flags = append(g.Flags, "Add who received it, so gifts per person can be totalled.")
			}
		case string(trace.Lost):
			g.Flags = append(g.Flags, "Ask your tax professional whether this loss can be deducted; keep any police report or exchange notice.")
		}
		r.Gifts = append(r.Gifts, *g)
	}
	for i := range r.Gifts {
		g := &r.Gifts[i]
		if g.Category != string(trace.Gift) {
			continue
		}
		yr := time.Unix(g.Time, 0).In(z).Year()
		if total := given[[2]string{fmt.Sprint(yr), g.Recipient}]; g.Recipient != "" && total > exclusionFor(yr) {
			g.Flags = append(g.Flags, fmt.Sprintf("Gifts to %s in %d total %s, above the %s annual exclusion: a gift tax return (Form 709) is likely due.",
				g.Recipient, yr, usdText(total), usdText(exclusionFor(yr))))
		}
	}
	// Gifts received: carryover basis, and the lower value for losses.
	for k, l := range p.labels {
		if l.Category != trace.GiftIn {
			continue
		}
		it, ok := p.tr.Items[k]
		if !ok {
			continue
		}
		g := GiftRow{Key: k, Time: it.Time.Unix(), Account: it.Account, Category: string(trace.GiftIn), Sats: it.Sats, Basis: l.FiatCents,
			Recipient: l.Recipient, Note: l.Note, Flags: []string{}}
		if pr, ok := p.prices.lookup(it.Time.In(z)); ok {
			g.Value = lots.MulDiv(pr, it.Sats, 100_000_000)
		}
		if !l.AcquiredAt.IsZero() {
			g.Acquired = l.AcquiredAt.In(z).Format("2006-01-02")
		} else {
			g.Flags = append(g.Flags, "Without the donor's purchase date, the holding period starts the day after you received it.")
		}
		if l.FiatCents == 0 {
			g.Flags = append(g.Flags, "Without the donor's basis, the basis is $0: ask the donor for it.")
		}
		if g.Value < g.Basis {
			g.Flags = append(g.Flags, fmt.Sprintf("Worth less (%s) than the donor's basis when received: for a loss, the basis is that lower value.", usdText(g.Value)))
		}
		r.Gifts = append(r.Gifts, g)
	}
	sort.SliceStable(r.Gifts, func(i, j int) bool { return r.Gifts[i].Time < r.Gifts[j].Time })

	for i := range r.Years {
		y := &r.Years[i]
		w := counts[y.Year]
		y.DigitalAsset = DigitalAssetQuestion{Answer: "No", Reasons: []string{}}
		if w == nil {
			continue
		}
		add := func(n int, one, many string) {
			if n > 0 {
				y.DigitalAsset.Answer = "Yes"
				if n == 1 {
					y.DigitalAsset.Reasons = append(y.DigitalAsset.Reasons, one)
				} else {
					y.DigitalAsset.Reasons = append(y.DigitalAsset.Reasons, fmt.Sprintf(many, n))
				}
			}
		}
		add(w.sales, "1 sale or spend", "%d sales or spends")
		add(w.income, "1 income receipt", "%d income receipts")
		add(w.fees, "a network fee paid in BTC on 1 transfer between your own wallets", "network fees paid in BTC on %d transfers between your own wallets")
		if w.given > 0 {
			y.DigitalAsset.Ask = append(y.DigitalAsset.Ask, fmt.Sprintf("%d gift%s, donation%s or loss%s: ask your tax professional how these affect the answer",
				w.given, plural(w.given), plural(w.given), map[bool]string{true: "es", false: ""}[w.given != 1]))
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// postLabelDetails sets an income type or a gift's recipient on a record,
// keeping the rest of its label. Imported income gets an income label
// that carries only the type.
func (s *Server) postLabelDetails(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var in struct {
		IncomeType *string
		Recipient  *string
	}
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if in.IncomeType != nil {
		if _, ok := trace.IncomeTypes[*in.IncomeType]; !ok {
			fail(w, 400, fmt.Errorf("unknown income type %q", *in.IncomeType))
			return
		}
	}
	labels, err := s.st.Labels()
	if err != nil {
		fail(w, 500, err)
		return
	}
	l, ok := labels[key]
	if !ok {
		if in.IncomeType == nil || !strings.HasPrefix(key, "ex:") {
			fail(w, 400, errors.New("label the record first"))
			return
		}
		l = trace.Label{Key: key, Category: trace.IncomeIn}
	}
	if in.IncomeType != nil {
		l.IncomeType = *in.IncomeType
	}
	if in.Recipient != nil {
		l.Recipient = strings.TrimSpace(*in.Recipient)
	}
	if _, err := s.record("user", "label", "Changed a record's details", 0, []change{{Kind: "label", Key: key}}, func() error { return s.st.SetLabel(l) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, l)
}
