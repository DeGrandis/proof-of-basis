package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lots"
	"github.com/DeGrandis/proof-of-basis/internal/ollama"
	"github.com/DeGrandis/proof-of-basis/internal/store"
	"github.com/DeGrandis/proof-of-basis/internal/tax"
	"github.com/DeGrandis/proof-of-basis/internal/trace"
)

// The assistant's tools after consolidation: a few general building blocks
// instead of one tool per question. The older single-purpose tools still
// answer if called by name (saved chats, tests), but aren't offered.

func toolSpecs() []ollama.Tool {
	t := func(name, desc string, params map[string]any) ollama.Tool {
		return ollama.Tool{Type: "function", Function: ollama.ToolSpec{Name: name, Description: desc, Parameters: params}}
	}
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	arr := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	specs := []ollama.Tool{
		analyzeToolSpec(),
		t("get_record", "Everything about one record by id or txid: amounts, fee, label, where its coins came from and went (trace), the lots a sale used, and for a Review item the likely other sides of a transfer. onchain=true adds inputs/outputs from the user's Fulcrum server.", obj(map[string]any{
			"id":      str("record id (as in [ex:12]) or txid"),
			"onchain": boolean("also fetch on-chain details from Fulcrum"),
		}, "id")),
		t("accounts", "Accounts: with no arguments, every account with balance, cost basis, wallets' scan dates and exchanges' imported files. With name: that account's lots and latest records (sources=true: where its coins came from). With date: holdings and cost basis per account on that day.", obj(map[string]any{
			"name":    str("one account (optional)"),
			"date":    str("YYYY-MM-DD for holdings on that day (optional)"),
			"sources": boolean("where coins came from: with name, that account's; without, all the bitcoin held now (\"how did I get my btc?\")"),
		})),
		t("get_tax_year", "A tax year's totals and its sales (largest gains and losses first), short/long term and income.", obj(map[string]any{
			"year": num("the year"),
		}, "year")),
		t("simulate_sale", "Estimate selling bitcoin today: proceeds, cost basis, short/long-term gain and the extra tax for this year. "+
			"One account: account + btc, usd or all=true; choose coins with strategy or lots. Everything: account \"all\" with all=true. "+
			"Work backwards: target_gain or target_tax (USD) finds the most you can sell staying at or under it (cheapest-tax coins first; account optional).", obj(map[string]any{
			"account":     str("account name, or \"all\" for every account"),
			"btc":         num("amount in BTC"),
			"usd":         num("amount in USD"),
			"all":         boolean("sell everything in the account (or in every account with account \"all\")"),
			"strategy":    str("which coins: highest_cost, lowest_cost, oldest, newest, long_term_first"),
			"lots":        arr("specific lot ids to sell in full"),
			"target_gain": num("find the most you can sell with total gain at or under this USD amount"),
			"target_tax":  num("find the most you can sell with extra tax at or under this USD amount"),
		})),
		t("price", "BTC price in USD: on one date, or over a range (high, low, average, change).", obj(map[string]any{
			"date": str("YYYY-MM-DD"),
			"from": str("range start YYYY-MM-DD"),
			"to":   str("range end YYYY-MM-DD (default today)"),
		})),
		t("calculate", "Exact arithmetic: + - * / and parentheses. Use it for any sum, difference or comparison instead of doing math yourself.", obj(map[string]any{
			"expression": str("for example (1302.61 + 163.51)"),
		}, "expression")),
		t("data_health", "Problems worth fixing: stale imports, old wallet scans, sales without cost basis, balance mismatches, Review items, warnings.", obj(map[string]any{})),
	}
	specs = append(specs, actionToolSpecs()...)
	specs = append(specs, t("undo_action", "Undo a change from Activity (the user's or yours). With no id, the most recent change you made in this chat, else the most recent change overall. The user confirms with the Undo button unless they turned confirmation off.", obj(map[string]any{
		"id": num("activity number (optional)"),
	})))
	return specs
}

// runConsolidated handles the consolidated tools; ok is false otherwise.
func (tb *toolbox) runConsolidated(name string, args map[string]any, lk *Lookup) (out string, ok bool, err error) {
	switch name {
	case "analyze_records":
		out, lk.Summary, err = tb.analyze(args, lk)
	case "get_record":
		out, lk.Summary, err = tb.getRecord(args, lk)
	case "accounts":
		out, lk.Summary, err = tb.accountsTool(args)
		lk.Link = "#overview"
	case "simulate_sale":
		out, lk.Summary, err = tb.simulate(args)
	case "price":
		out, lk.Summary, err = tb.priceTool(args)
	case "undo_action":
		out, lk.Summary, err = tb.undo(args, lk)
	default:
		return "", false, nil
	}
	return out, true, err
}

// --- one record ---

func (tb *toolbox) getRecord(a map[string]any, lk *Lookup) (string, string, error) {
	id := strings.Trim(argStr(a, "id"), "[]` ")
	if id == "" {
		return "", "", errors.New("give a record id or txid")
	}
	var b strings.Builder
	b.WriteString(eventDetail(tb.rep, id))
	if out, _, err := tb.trace(id, ""); err == nil {
		// skip the first line (the record again)
		if i := strings.Index(out, "\n"); i >= 0 {
			b.WriteString(out[i+1:])
		}
	}
	for _, it := range tb.rep.Unmatched {
		if sameRecord(it.Key, it.Txid, id) {
			if out, _, err := tb.matchCandidates(id); err == nil {
				b.WriteString("Waiting in Review. " + out)
			}
			break
		}
	}
	if on, _ := a["onchain"].(bool); on {
		txid := linkArg(tb.rep, id)
		if out, _, err := tb.onchain(txid); err == nil {
			b.WriteString(out)
		} else {
			fmt.Fprintf(&b, "On-chain details unavailable: %v\n", err)
		}
	}
	lk.Link = "#tx/" + linkArg(tb.rep, id)
	return b.String(), "Looked up " + recordName(tb.rep, id), nil
}

// --- accounts ---

// sourcesOverall answers "how did I get my bitcoin?": the coins held now,
// grouped by how and where each was first acquired.
func (tb *toolbox) sourcesOverall() (string, string, error) {
	origin := map[string]GraphEvent{}
	for _, g := range tb.rep.Graph {
		origin[g.Key] = g
	}
	type key struct{ how, acct string }
	sums := map[key]*[2]int64{}
	var total int64
	for _, l := range tb.rep.OpenLots {
		if l.Sats <= 0 || l.Account == trace.UntrackedAccount {
			continue
		}
		k := key{"other", ""}
		if g, ok := origin[l.OriginID]; ok {
			k = key{kindName(g.Kind), g.Account}
		}
		if sums[k] == nil {
			sums[k] = &[2]int64{}
		}
		sums[k][0] += l.Sats
		sums[k][1] += l.Basis
		total += l.Sats
	}
	keys := make([]key, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return sums[keys[i]][0] > sums[keys[j]][0] })
	var b strings.Builder
	fmt.Fprintf(&b, "Where the %s held now came from (how and where each coin was first acquired, before any moves between accounts):\n", btcStr(total))
	for _, k := range keys {
		where := ""
		if k.acct != "" {
			where = " in " + k.acct
		}
		fmt.Fprintf(&b, "- %s%s: %s, cost basis %s\n", k.how, where, btcStr(sums[k][0]), usdStr(sums[k][1]))
	}
	return b.String(), "Traced where your bitcoin came from", nil
}

func (tb *toolbox) accountsTool(a map[string]any) (string, string, error) {
	if d := argStr(a, "date"); d != "" {
		return tb.holdingsOn(d)
	}
	if n := argStr(a, "name"); n != "" {
		name, err := tb.resolveAccount(n)
		if err != nil {
			return "", "", err
		}
		if src, _ := a["sources"].(bool); src {
			return tb.trace("", name)
		}
		return tb.account(name)
	}
	if src, _ := a["sources"].(bool); src {
		return tb.sourcesOverall()
	}
	var b strings.Builder
	imports := map[string][]store.Import{}
	if ims, err := tb.s.st.Imports(); err == nil {
		for _, im := range ims {
			imports[im.Account] = append(imports[im.Account], im)
		}
	}
	newest := map[string]time.Time{}
	if ex, err := tb.s.st.ExchangeEvents(); err == nil {
		for _, e := range ex {
			if e.Time.After(newest[e.Account]) {
				newest[e.Account] = e.Time
			}
		}
	}
	for _, x := range tb.rep.Accounts {
		fmt.Fprintf(&b, "- %s (%s): %s, cost basis %s", x.Name, x.Kind, btcStr(x.Sats), usdStr(x.Basis))
		if x.Kind == "wallet" {
			if x.Script != "" {
				fmt.Fprintf(&b, ", %s", x.Script)
			}
			if x.ScannedAt != "" {
				fmt.Fprintf(&b, ", last scanned %s", x.ScannedAt)
			}
		}
		if ims := imports[x.Name]; len(ims) > 0 {
			var files []string
			for _, im := range ims {
				files = append(files, fmt.Sprintf("%s (%d rows, imported %s)", im.Filename, im.Rows, im.ImportedAt))
			}
			fmt.Fprintf(&b, "; files: %s", strings.Join(files, ", "))
		}
		if t, ok := newest[x.Name]; ok {
			fmt.Fprintf(&b, "; newest record %s", dayOf(t))
		}
		b.WriteString("\n")
	}
	return b.String(), "Looked up accounts", nil
}

// --- prices ---

func (tb *toolbox) priceTool(a map[string]any) (string, string, error) {
	if d := argStr(a, "date"); d != "" && argStr(a, "from") == "" {
		return tb.price(d)
	}
	from, ok := parseDay(argStr(a, "from"))
	if !ok {
		return "", "", errors.New("give date, or from (and to) as YYYY-MM-DD")
	}
	to, ok := parseDay(argStr(a, "to"))
	if !ok {
		to = time.Now().UTC().Truncate(24 * time.Hour)
	}
	if to.Before(from) || to.Sub(from) > 20*366*24*time.Hour {
		return "", "", errors.New("give a range of up to 20 years")
	}
	p, err := tb.s.pipeline()
	if err != nil {
		return "", "", err
	}
	var hi, lo, first, last int64
	var hiDay, loDay, firstDay, lastDay string
	var sum float64
	n := 0
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		c, ok := p.prices.lookup(d)
		if !ok {
			continue
		}
		day := dayOf(d)
		if n == 0 {
			first, firstDay, lo, loDay = c, day, c, day
		}
		if c > hi {
			hi, hiDay = c, day
		}
		if c < lo {
			lo, loDay = c, day
		}
		last, lastDay = c, day
		sum += float64(c)
		n++
	}
	if n == 0 {
		return "", "", errors.New("no prices in that range")
	}
	change := 100 * float64(last-first) / float64(first)
	return fmt.Sprintf("BTC price from %s to %s (%d days): high %s on %s, low %s on %s, average %s, first %s, last %s (%+.1f%%).",
		firstDay, lastDay, n, usdStr(hi), hiDay, usdStr(lo), loDay, usdStr(int64(sum/float64(n))), usdStr(first), usdStr(last), change), "Looked up prices " + firstDay + " to " + lastDay, nil
}

// --- selling ---

func (tb *toolbox) simulate(a map[string]any) (string, string, error) {
	acct := argStr(a, "account")
	all, _ := a["all"].(bool)
	if _, ok := a["target_gain"]; ok {
		return tb.solve(a, "gain", argNum(a, "target_gain"))
	}
	if _, ok := a["target_tax"]; ok {
		return tb.solve(a, "tax", argNum(a, "target_tax"))
	}
	if strings.EqualFold(acct, "all") || acct == "" && all {
		if !all && (argNum(a, "btc") > 0 || argNum(a, "usd") > 0) {
			return tb.compareAccounts(a)
		}
		return tb.sellEverything()
	}
	if acct == "" {
		return "", "", errors.New("name the account to sell from, or \"all\" for everything")
	}
	if a["strategy"] != nil || a["lots"] != nil {
		return tb.whatIfPicks(a)
	}
	return tb.whatIf(a)
}

// compareAccounts answers "if I sell 0.1 BTC" with no account named:
// basis is tracked per wallet, so it sells the amount from each account
// that holds enough and sets the results side by side.
func (tb *toolbox) compareAccounts(a map[string]any) (string, string, error) {
	sats := int64(math.Round(argNum(a, "btc") * 1e8))
	if sats <= 0 && tb.rep.Price > 0 {
		sats = int64(math.Round(argNum(a, "usd") * 100 * 1e8 / float64(tb.rep.Price)))
	}
	if sats <= 0 {
		return "", "", errors.New("give the amount to sell in btc or usd")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Selling %s needs one account (cost basis is tracked per wallet). Sold today from each account that holds enough:\n", btcStr(sats))
	n := 0
	for _, acc := range tb.rep.Accounts {
		if acc.Sats < sats {
			continue
		}
		r, err := tb.simulateSales([]saleSpec{{Account: acc.Name, Sats: sats}})
		if err != nil {
			return "", "", err
		}
		short, long := r.Short[1]-r.Short[2], r.Long[1]-r.Long[2]
		fmt.Fprintf(&b, "- %s: proceeds %s, gain %s (short-term %s, long-term %s), adds %s to %d's estimated tax\n",
			acc.Name, usdStr(r.Short[1]+r.Long[1]), usdStr(short+long), usdStr(short), usdStr(long), usdStr(r.After-r.Before), time.Now().Year())
		n++
	}
	if n == 0 {
		return "", "", fmt.Errorf("no single account holds %s; name the accounts to sell from", btcStr(sats))
	}
	b.WriteString("Show the user these options and ask which account they mean (or point out the cheapest).\n")
	return b.String(), fmt.Sprintf("Compared selling %s by account", btcStr(sats)), nil
}

type saleSpec struct {
	Account string
	Sats    int64
	Picks   []lots.Pick
}

type simResult struct {
	Price, Before, After int64
	PriceDay             string
	Short, Long          [3]int64 // sats, proceeds, basis
	Missing              int64
	PerAccount           map[string]*[3]int64
	ShortRate, LongRate  string
}

// simulateSales sells the given amounts today, all together, and works out
// the year's extra tax on the total.
func (tb *toolbox) simulateSales(specs []saleSpec) (*simResult, error) {
	p, err := tb.s.pipeline()
	if err != nil {
		return nil, err
	}
	day, price, ok := p.prices.latest()
	if !ok || price <= 0 {
		return nil, errors.New("no BTC price yet: update prices in Settings")
	}
	base, err := lots.Run(p.tr.Ops, p.cfg)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ops := append([]lots.Op(nil), p.tr.Ops...)
	picks := false
	for _, s := range specs {
		ops = append(ops, lots.Op{ID: "sim:" + s.Account, Kind: lots.Dispose, Time: now, Account: s.Account, Sats: s.Sats, Cents: lots.MulDiv(price, s.Sats, 100_000_000), Picks: s.Picks})
		picks = picks || len(s.Picks) > 0
	}
	cfg := p.cfg
	if picks {
		yearMethod := cfg.Method
		cfg.Method = func(y int) tax.Method {
			if y == now.Year() {
				return tax.Specific
			}
			return yearMethod(y)
		}
	}
	res, err := lots.Run(ops, cfg)
	if err != nil {
		return nil, err
	}
	var yst, ylt int64
	for _, d := range base.Disposals {
		if d.Disposed.Year() == now.Year() {
			if d.LongTerm {
				ylt += d.Gain()
			} else {
				yst += d.Gain()
			}
		}
	}
	for _, inc := range p.tr.Income {
		if inc.Time.Year() == now.Year() {
			yst += inc.Cents
		}
	}
	r := &simResult{Price: price, PriceDay: day, PerAccount: map[string]*[3]int64{}, ShortRate: p.set["rateShort"], LongRate: p.set["rateLong"]}
	for _, d := range res.Disposals {
		acct, ok := strings.CutPrefix(d.OpID, "sim:")
		if !ok {
			continue
		}
		side := &r.Short
		if d.LongTerm {
			side = &r.Long
		}
		side[0] += d.Sats
		side[1] += d.Proceeds
		side[2] += d.Basis
		if r.PerAccount[acct] == nil {
			r.PerAccount[acct] = &[3]int64{}
		}
		r.PerAccount[acct][0] += d.Sats
		r.PerAccount[acct][1] += d.Proceeds
		r.PerAccount[acct][2] += d.Basis
		if d.Missing {
			r.Missing += d.Sats
		}
	}
	sbp, lbp := rateBP(p.set, "rateShort"), rateBP(p.set, "rateLong")
	r.Before = yearTax(yst, ylt, sbp, lbp)
	r.After = yearTax(yst+r.Short[1]-r.Short[2], ylt+r.Long[1]-r.Long[2], sbp, lbp)
	return r, nil
}

// solve finds the most bitcoin that can be sold today while the gain (or
// the extra tax) stays at or under a limit, taking the coins that add the
// least gain first.
func (tb *toolbox) solve(a map[string]any, what string, limitUSD float64) (string, string, error) {
	price := tb.rep.Price
	if price <= 0 {
		return "", "", errors.New("no BTC price yet: update prices in Settings")
	}
	limit := int64(math.Round(limitUSD * 100))
	acct := argStr(a, "account")
	if strings.EqualFold(acct, "all") {
		acct = ""
	}
	if acct != "" {
		n, err := tb.resolveAccount(acct)
		if err != nil {
			return "", "", err
		}
		acct = n
	}
	type cand struct {
		l    lots.Lot
		gpb  float64 // gain per sat
		long bool
	}
	now := time.Now()
	var cs []cand
	for _, l := range tb.rep.OpenLots {
		if l.Sats <= 0 || l.Account == trace.UntrackedAccount || acct != "" && l.Account != acct {
			continue
		}
		g := float64(lots.MulDiv(price, l.Sats, 100_000_000)-l.Basis) / float64(l.Sats)
		cs = append(cs, cand{l, g, now.Sub(l.Acquired) > 365*24*time.Hour})
	}
	if len(cs) == 0 {
		return "", "", errors.New("no coins to sell")
	}
	// Least gain per coin first; at equal gain, long-term first (taxed less).
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].gpb != cs[j].gpb {
			return cs[i].gpb < cs[j].gpb
		}
		return cs[i].long && !cs[j].long
	})
	// This year's results so far, to price the extra tax.
	var yst, ylt int64
	thisYear := now.UTC().Year()
	for _, y := range tb.rep.Years {
		if y.Year == thisYear {
			yst = y.ShortProceeds - y.ShortBasis + y.Income
			ylt = y.LongProceeds - y.LongBasis
		}
	}
	sbp, lbp := rateBP(tb.rep.Settings, "rateShort"), rateBP(tb.rep.Settings, "rateLong")
	baseTax := yearTax(yst, ylt, sbp, lbp)
	cost := func(st, lt int64) int64 {
		if what == "gain" {
			return st + lt
		}
		return yearTax(yst+st, ylt+lt, sbp, lbp) - baseTax
	}
	var st, lt int64
	taken := map[string][]lots.Pick{}
	var total int64
	var lastAcct string
	var lastGain float64
	for _, c := range cs {
		g := func(sats int64) int64 { return int64(math.Round(c.gpb * float64(sats))) }
		add := func(sats int64) (int64, int64) {
			if c.long {
				return st, lt + g(sats)
			}
			return st + g(sats), lt
		}
		if s2, l2 := add(c.l.Sats); cost(s2, l2) <= limit {
			st, lt = s2, l2
			taken[c.l.Account] = append(taken[c.l.Account], lots.Pick{LotID: c.l.ID, Sats: c.l.Sats})
			total += c.l.Sats
			lastAcct, lastGain = c.l.Account, c.gpb
			continue
		}
		// part of this lot: the most sats that still fit
		lo, hi := int64(0), c.l.Sats
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if s2, l2 := add(mid); cost(s2, l2) <= limit {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo > 0 {
			st, lt = add(lo)
			taken[c.l.Account] = append(taken[c.l.Account], lots.Pick{LotID: c.l.ID, Sats: lo})
			total += lo
			lastAcct, lastGain = c.l.Account, c.gpb
		}
		break
	}
	label := map[string]string{"gain": "gain", "tax": "extra tax"}[what]
	if total == 0 {
		return fmt.Sprintf("Nothing can be sold today while keeping the %s at or under %s; even the cheapest coins would add more.", label, usdStr(limit)), "Worked out how much you can sell", nil
	}
	var specs []saleSpec
	for a, picks := range taken {
		var sats int64
		for _, p := range picks {
			sats += p.Sats
		}
		specs = append(specs, saleSpec{Account: a, Sats: sats, Picks: picks})
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Account < specs[j].Account })
	r, err := tb.simulateSales(specs)
	if err != nil {
		return "", "", err
	}
	// The estimate per coin can be off by a few cents from the engine's
	// rounding; trim the last coins taken until the real figure fits.
	actual := func(r *simResult) int64 {
		if what == "gain" {
			return r.Short[1] + r.Long[1] - r.Short[2] - r.Long[2]
		}
		return r.After - r.Before
	}
	for tries := 0; tries < 8 && actual(r) > limit && total > 0; tries++ {
		last := &specs[len(specs)-1]
		for i := range specs {
			if specs[i].Account == lastAcct {
				last = &specs[i]
			}
		}
		p := &last.Picks[len(last.Picks)-1]
		cut := max(int64(math.Ceil(float64(actual(r)-limit)/math.Max(lastGain, 1e-9)))+1000, 1000)
		cut = min(cut, p.Sats)
		p.Sats -= cut
		last.Sats -= cut
		total -= cut
		if p.Sats == 0 {
			last.Picks = last.Picks[:len(last.Picks)-1]
		}
		if r, err = tb.simulateSales(specs); err != nil {
			return "", "", err
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The most you can sell today at %s per BTC while keeping the %s at or under %s: %s", usdStr(r.Price), label, usdStr(limit), btcStr(total))
	if total == func() int64 {
		var s int64
		for _, c := range cs {
			s += c.l.Sats
		}
		return s
	}() {
		b.WriteString(" (that's everything")
		if acct != "" {
			b.WriteString(" in " + acct)
		}
		b.WriteString(")")
	}
	b.WriteString(", choosing the coins that add the least gain first (losses, then the highest-cost coins).\n")
	writeSim(&b, r)
	b.WriteString("Selling specific coins like this needs them identified at the time of sale (specific identification).\n")
	return b.String(), "Worked out how much you can sell", nil
}

func writeSim(b *strings.Builder, r *simResult) {
	fmt.Fprintf(b, "Proceeds %s, cost basis %s, gain %s (short-term %s on %s, long-term %s on %s).\n",
		usdStr(r.Short[1]+r.Long[1]), usdStr(r.Short[2]+r.Long[2]), usdStr(r.Short[1]+r.Long[1]-r.Short[2]-r.Long[2]),
		usdStr(r.Short[1]-r.Short[2]), btcStr(r.Short[0]), usdStr(r.Long[1]-r.Long[2]), btcStr(r.Long[0]))
	fmt.Fprintf(b, "Estimated tax for this year goes from %s to %s: %s extra (rates in Settings: short-term %s%%, long-term %s%%).\n", usdStr(r.Before), usdStr(r.After), usdStr(r.After-r.Before), r.ShortRate, r.LongRate)
	if len(r.PerAccount) > 1 {
		var names []string
		for a := range r.PerAccount {
			names = append(names, a)
		}
		sort.Strings(names)
		b.WriteString("By account:\n")
		for _, a := range names {
			x := r.PerAccount[a]
			fmt.Fprintf(b, "- %s: %s, proceeds %s, gain %s\n", a, btcStr(x[0]), usdStr(x[1]), usdStr(x[1]-x[2]))
		}
	}
	if r.Missing > 0 {
		fmt.Fprintf(b, "%s would have no known cost basis.\n", btcStr(r.Missing))
	}
}

// --- undo ---

func (tb *toolbox) undo(a map[string]any, lk *Lookup) (string, string, error) {
	var target store.Action
	if id := int64(argNum(a, "id")); id > 0 {
		x, err := tb.s.st.Action(id)
		if err != nil {
			return "", "", err
		}
		target = x
	} else {
		list, err := tb.s.st.Actions("", 200)
		if err != nil {
			return "", "", err
		}
		// A proposal in this chat that wasn't applied yet: nothing changed,
		// so it's simply withdrawn.
		for _, x := range list {
			if x.Status == "pending" && x.ChatID == tb.chatID {
				tb.s.st.UpdateAction(x.ID, "dismissed", nil)
				tb.actions = append(tb.actions, x.ID)
				lk.Link = "#activity"
				return fmt.Sprintf("Withdrew the proposal \"%s\" (activity #%d). It was never applied, so nothing changed.", x.Summary, x.ID), "Withdrew: " + x.Summary, nil
			}
		}
		for _, x := range list { // this chat's own changes first
			if x.Status == "done" && x.Undoable && x.Actor == "assistant" && x.ChatID == tb.chatID {
				target = x
				break
			}
		}
		if target.ID == 0 {
			for _, x := range list {
				if x.Status == "done" && x.Undoable {
					target = x
					break
				}
			}
		}
		if target.ID == 0 {
			return "", "", errors.New("there's nothing to undo")
		}
	}
	if target.Status != "done" || !target.Undoable {
		return "", "", fmt.Errorf("activity #%d (%s) is %s and can't be undone", target.ID, target.Summary, target.Status)
	}
	lk.Link = "#activity"
	if tb.s.st.Setting("aiAutoApply", "false") == "true" {
		req := httptest.NewRequest("POST", "/api/actions/"+strconv.FormatInt(target.ID, 10)+"/undo", strings.NewReader("{}"))
		req.SetPathValue("id", strconv.FormatInt(target.ID, 10))
		rec := httptest.NewRecorder()
		tb.s.undoAction(rec, req)
		if rec.Code != 200 {
			var e struct{ Error string }
			json.Unmarshal(rec.Body.Bytes(), &e)
			return "", "", errors.New(e.Error)
		}
		tb.actions = append(tb.actions, target.ID)
		return fmt.Sprintf("Undone: %s (activity #%d). It can be redone from Activity.", target.Summary, target.ID), "Undid: " + target.Summary, nil
	}
	tb.actions = append(tb.actions, target.ID) // shows the change's card, with its Undo button
	return fmt.Sprintf("NOT UNDONE YET. The user now sees activity #%d (%s) with an Undo button and must press it. Say exactly that: press Undo on the card to undo it.", target.ID, target.Summary), "Offered to undo: " + target.Summary, nil
}

// --- CSV exports ---

func (s *Server) addExport(name string, data []byte) string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	if err := s.st.AddExport(tok, name, data, time.Now()); err != nil {
		return ""
	}
	return tok
}

func (s *Server) getExport(w http.ResponseWriter, r *http.Request) {
	name, data, err := s.st.Export(r.PathValue("token"))
	if err != nil {
		http.Error(w, "this file isn't available any more; ask the assistant again", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(data)
}

// --- bulk labels by description ---

// matchReview picks Review items by filters, for label_items' match.
func (tb *toolbox) matchReview(m map[string]any) ([]trace.Item, error) {
	acct := argStr(m, "account")
	if acct != "" {
		n, err := tb.resolveAccount(acct)
		if err != nil {
			return nil, err
		}
		acct = n
	}
	dir := strings.ToLower(argStr(m, "direction"))
	network := strings.ToLower(argStr(m, "network"))
	text := strings.ToLower(argStr(m, "text"))
	from, hasFrom := parseDay(argStr(m, "from"))
	to, hasTo := parseDay(argStr(m, "to"))
	_, hasMin := m["min_usd"]
	_, hasMax := m["max_usd"]
	lo, hi := int64(argNum(m, "min_usd")*100), int64(argNum(m, "max_usd")*100)
	var out []trace.Item
	for _, it := range tb.rep.Unmatched {
		switch {
		case acct != "" && it.Account != acct,
			dir != "" && string(it.Dir) != dir,
			network != "" && !strings.EqualFold(it.Network, network),
			text != "" && !strings.Contains(strings.ToLower(it.Note+" "+it.Address+" "+it.Txid+" "+it.Key), text),
			hasFrom && it.Time.Before(from), hasTo && it.Time.After(to.Add(24*time.Hour-time.Second)),
			hasMin && it.Value < lo, hasMax && it.Value > hi:
			continue
		}
		out = append(out, it)
	}
	return out, nil
}
