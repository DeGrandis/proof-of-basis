package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lots"
	"github.com/DeGrandis/proof-of-basis/internal/ollama"
	"github.com/DeGrandis/proof-of-basis/internal/trace"
)

// More read-only tools for the assistant: totals, lot timing, matching
// help, holdings on a date, exact math, data health, on-chain details,
// income, Lightning, and tracing coins from source to outcome.

func moreToolSpecs() []ollama.Tool {
	t := func(name, desc string, params map[string]any) ollama.Tool {
		return ollama.Tool{Type: "function", Function: ollama.ToolSpec{Name: name, Description: desc, Parameters: params}}
	}
	arr := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	return []ollama.Tool{
		analyzeToolSpec(),
		t("calculate", "Exact arithmetic. Use it for any sum, difference, percentage or comparison of numbers instead of doing math yourself. Supports + - * / and parentheses.", obj(map[string]any{
			"expression": str("for example (8672.03 - 1090.09) or -81.11 - (-76.10)"),
		}, "expression")),
		t("holdings_on", "BTC held on a date: total and per account, its value at that day's price, and its cost basis.", obj(map[string]any{
			"date": str("YYYY-MM-DD"),
		}, "date")),
		t("lots_turning_long_term", "Purchase lots still held that become long-term (held over a year) within the next N days, with the date each one turns.", obj(map[string]any{
			"within_days": num("look-ahead in days, default 60"),
			"account":     str("only this account (optional)"),
		})),
		t("lots_at_a_loss", "Purchase lots still held whose value today is below their cost basis, with the unrealized loss on each and in total (candidates for tax-loss harvesting).", obj(map[string]any{
			"account": str("only this account (optional)"),
		})),
		t("review_match_candidates", "For one item waiting in Review: records in the user's other accounts that could be the other side of a transfer (similar amount, close in time), and why it didn't match automatically.", obj(map[string]any{
			"id": str("the Review item's record id or txid"),
		}, "id")),
		t("data_health", "Problems worth fixing: stale exchange imports, wallets not scanned lately, sales without a cost basis, balance mismatches, items waiting in Review, warnings.", obj(map[string]any{})),
		t("onchain_transaction", "Details of an on-chain transaction from the user's own Fulcrum server: inputs, outputs, which addresses are the user's, fee, block. Needs Fulcrum connected.", obj(map[string]any{
			"txid": str("64-character transaction id"),
		}, "txid")),
		t("income_records", "Income received (interest, rewards, Lightning routing fees and the like) in a year: each record with BTC and USD value.", obj(map[string]any{
			"year": num("the year"),
		}, "year")),
		t("lightning_activity", "The Lightning node's activity between two dates: payments sent and received, routing fees earned, channel opens and closes.", obj(map[string]any{
			"from": str("first day, YYYY-MM-DD (optional)"),
			"to":   str("last day, YYYY-MM-DD (optional)"),
		})),
		t("trace_coins", "Follow coins in plain terms. For a purchase or income record: where those coins went (sold, spent, gifted, still held and where). For a sale, spend or gift: which purchases the coins came from and the likely route between accounts. For an account: where the coins it holds came from.", obj(map[string]any{
			"id":      str("a record id or txid (use this or account)"),
			"account": str("an account name (use this or id)"),
		})),
		t("what_if_sale_picks", "Like what_if_sale but choosing which coins to sell: by strategy (highest_cost, lowest_cost, oldest, newest, long_term_first) or by specific lot ids from get_account. Use it to find the sale with the least tax.", obj(map[string]any{
			"account":  str("account to sell from"),
			"btc":      num("amount in BTC (with a strategy)"),
			"usd":      num("amount in USD (with a strategy)"),
			"strategy": str("highest_cost, lowest_cost, oldest, newest or long_term_first"),
			"lots":     arr("specific lot ids to sell in full (instead of a strategy)"),
		}, "account")),
	}
}

// runMore handles the tools above; ok is false for an unknown name.
func (tb *toolbox) runMore(name string, args map[string]any, lk *Lookup) (out string, ok bool, err error) {
	switch name {
	case "analyze_records", "summarize_records":
		if name == "summarize_records" && argStr(args, "stat") == "" {
			args["stat"] = "summary"
			if argStr(args, "source") == "" {
				args["source"] = "records"
			}
		}
		out, lk.Summary, err = tb.analyze(args, lk)
	case "calculate":
		var v float64
		expr := argStr(args, "expression")
		v, err = calc(expr)
		if err == nil {
			// Give it rounded with commas too: models garble big numbers
			// when they format them themselves.
			out = fmt.Sprintf("%s = %s (rounded: %s)", expr, strconv.FormatFloat(round6(v), 'f', -1, 64), strings.Replace(usdStr(int64(math.Round(v*100))), "$", "", 1))
			lk.Summary = "Calculated " + clip(expr, 30)
		}
	case "holdings_on":
		out, lk.Summary, err = tb.holdingsOn(argStr(args, "date"))
	case "lots_turning_long_term":
		out, lk.Summary = tb.turningLong(int(argNum(args, "within_days")), argStr(args, "account"))
		lk.Link = "#taxes"
	case "lots_at_a_loss":
		out, lk.Summary = tb.atLoss(argStr(args, "account"))
	case "review_match_candidates":
		out, lk.Summary, err = tb.matchCandidates(argStr(args, "id"))
		lk.Link = "#review"
	case "data_health":
		out, lk.Summary = tb.health()
	case "onchain_transaction":
		out, lk.Summary, err = tb.onchain(argStr(args, "txid"))
		if err == nil {
			lk.Link = "#tx/" + strings.ToLower(argStr(args, "txid"))
		}
	case "income_records":
		out, lk.Summary = tb.income(int(argNum(args, "year")))
	case "lightning_activity":
		out, lk.Summary, err = tb.lightningActivity(argStr(args, "from"), argStr(args, "to"))
		lk.Link = "#lightning"
	case "trace_coins":
		out, lk.Summary, err = tb.trace(argStr(args, "id"), argStr(args, "account"))
		lk.Link = "#graph"
	case "what_if_sale_picks":
		out, lk.Summary, err = tb.whatIfPicks(args)
	default:
		return "", false, nil
	}
	return out, true, err
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// --- exact math ---

// calc evaluates + - * / and parentheses over decimal numbers. Commas and
// dollar signs are ignored so model-written figures like "$1,234.56" work.
func calc(expr string) (float64, error) {
	s := strings.NewReplacer(",", "", "$", "", " ", "", "−", "-", "×", "*", "÷", "/").Replace(expr)
	if s == "" || len(s) > 500 {
		return 0, errors.New("give an arithmetic expression")
	}
	p := &parser{s: s}
	v, err := p.expr()
	if err != nil {
		return 0, err
	}
	if p.i != len(p.s) {
		return 0, fmt.Errorf("couldn't read %q", p.s[p.i:])
	}
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, errors.New("division by zero")
	}
	return v, nil
}

type parser struct {
	s     string
	i     int
	depth int
}

func (p *parser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *parser) expr() (float64, error) {
	v, err := p.term()
	for err == nil && (p.peek() == '+' || p.peek() == '-') {
		op := p.s[p.i]
		p.i++
		var r float64
		if r, err = p.term(); op == '+' {
			v += r
		} else {
			v -= r
		}
	}
	return v, err
}

func (p *parser) term() (float64, error) {
	v, err := p.factor()
	for err == nil && (p.peek() == '*' || p.peek() == '/' || p.peek() == '%') {
		op := p.s[p.i]
		p.i++
		var r float64
		r, err = p.factor()
		switch op {
		case '*':
			v *= r
		case '/':
			v /= r
		case '%':
			v = math.Mod(v, r)
		}
	}
	return v, err
}

func (p *parser) factor() (float64, error) {
	switch c := p.peek(); {
	case c == '-':
		p.i++
		v, err := p.factor()
		return -v, err
	case c == '+':
		p.i++
		return p.factor()
	case c == '(':
		if p.depth++; p.depth > 50 {
			return 0, errors.New("too many parentheses")
		}
		p.i++
		v, err := p.expr()
		if err == nil && p.peek() != ')' {
			err = errors.New("missing )")
		}
		p.i++
		p.depth--
		return v, err
	}
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '.') {
		p.i++
	}
	if start == p.i {
		return 0, fmt.Errorf("expected a number at %q", p.s[start:])
	}
	return strconv.ParseFloat(p.s[start:p.i], 64)
}

// --- holdings on a date ---

func (tb *toolbox) holdingsOn(day string) (string, string, error) {
	t, ok := parseDay(day)
	if !ok {
		return "", "", errors.New("give the date as YYYY-MM-DD")
	}
	end := t.Add(24*time.Hour - time.Second)
	p, err := tb.s.pipeline()
	if err != nil {
		return "", "", err
	}
	var ops []lots.Op
	for _, o := range p.tr.Ops {
		if !o.Time.After(end) {
			ops = append(ops, o)
		}
	}
	res, err := lots.Run(ops, p.cfg)
	if err != nil {
		return "", "", err
	}
	var sats, basis int64
	per := map[string][2]int64{}
	for _, l := range res.Open {
		sats += l.Sats
		basis += l.Basis
		v := per[l.Account]
		per[l.Account] = [2]int64{v[0] + l.Sats, v[1] + l.Basis}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "On %s: %s held, cost basis %s", day, btcStr(sats), usdStr(basis))
	if price, ok := p.prices.lookup(t); ok {
		fmt.Fprintf(&b, ", BTC price %s, worth %s", usdStr(price), usdStr(lots.MulDiv(price, sats, 100_000_000)))
	}
	b.WriteString(".\nBy account:\n")
	names := make([]string, 0, len(res.Balances))
	for a, s := range res.Balances {
		if s != 0 {
			names = append(names, a)
		}
	}
	sort.Slice(names, func(i, j int) bool { return res.Balances[names[i]] > res.Balances[names[j]] })
	for _, a := range names {
		fmt.Fprintf(&b, "- %s: %s", a, btcStr(res.Balances[a]))
		if v, ok := per[a]; ok {
			fmt.Fprintf(&b, ", cost basis %s", usdStr(v[1]))
		}
		b.WriteString("\n")
	}
	return b.String(), "Looked up holdings on " + day, nil
}

// --- lots ---

func (tb *toolbox) turningLong(days int, acct string) (string, string) {
	if days <= 0 {
		days = 60
	}
	if days > 400 {
		days = 400
	}
	now := time.Now().UTC()
	var b strings.Builder
	n := 0
	var total int64
	open := append([]lots.Lot(nil), tb.rep.OpenLots...)
	sort.Slice(open, func(i, j int) bool { return open[i].Acquired.Before(open[j].Acquired) })
	for _, l := range open {
		if l.Sats <= 0 || acct != "" && !strings.EqualFold(l.Account, acct) {
			continue
		}
		longOn := l.Acquired.AddDate(1, 0, 1) // held more than a year
		if longOn.Before(now) || longOn.After(now.AddDate(0, 0, days)) {
			continue
		}
		n++
		total += l.Sats
		if n <= toolRowCap {
			fmt.Fprintf(&b, "- lot %s in %s: %s acquired %s, basis %s, becomes long-term on %s (in %d days)\n", l.ID, l.Account, btcStr(l.Sats), dayOf(l.Acquired), usdStr(l.Basis), dayOf(longOn), int(longOn.Sub(now).Hours()/24)+1)
		}
	}
	if n == 0 {
		return fmt.Sprintf("No lots become long-term in the next %d days.", days), "Checked lots turning long-term"
	}
	return fmt.Sprintf("%d lots (%s) become long-term in the next %d days:\n", n, btcStr(total), days) + b.String(), fmt.Sprintf("Found %d lots turning long-term", n)
}

func (tb *toolbox) atLoss(acct string) (string, string) {
	price := tb.rep.Price
	if price <= 0 {
		return "No current BTC price, so losses can't be worked out. Update prices in Settings.", "Checked lots at a loss"
	}
	type lossLot struct {
		l    lots.Lot
		loss int64
	}
	var list []lossLot
	var total int64
	for _, l := range tb.rep.OpenLots {
		if l.Sats <= 0 || acct != "" && !strings.EqualFold(l.Account, acct) {
			continue
		}
		v := lots.MulDiv(price, l.Sats, 100_000_000)
		if v < l.Basis {
			list = append(list, lossLot{l, l.Basis - v})
			total += l.Basis - v
		}
	}
	if len(list) == 0 {
		return fmt.Sprintf("No lots are below cost at today's price of %s.", usdStr(price)), "Checked lots at a loss"
	}
	sort.Slice(list, func(i, j int) bool { return list[i].loss > list[j].loss })
	var b strings.Builder
	fmt.Fprintf(&b, "At %s per BTC, %d lots are below cost, %s of unrealized loss in total. Largest first:\n", usdStr(price), len(list), usdStr(total))
	now := time.Now()
	for i, x := range list {
		if i >= toolRowCap {
			fmt.Fprintf(&b, "(%d more)\n", len(list)-toolRowCap)
			break
		}
		term := "short-term"
		if now.Sub(x.l.Acquired) > 365*24*time.Hour {
			term = "long-term"
		}
		fmt.Fprintf(&b, "- lot %s in %s: %s acquired %s, basis %s, loss %s if sold today (%s)\n", x.l.ID, x.l.Account, btcStr(x.l.Sats), dayOf(x.l.Acquired), usdStr(x.l.Basis), usdStr(x.loss), term)
	}
	return b.String(), fmt.Sprintf("Found %d lots at a loss", len(list))
}

// --- review matching ---

func (tb *toolbox) matchCandidates(id string) (string, string, error) {
	var it *struct {
		key, acct, dir, txid, addr string
		t                          time.Time
		sats, fee                  int64
	}
	for _, u := range tb.rep.Unmatched {
		if sameRecord(u.Key, u.Txid, id) {
			it = &struct {
				key, acct, dir, txid, addr string
				t                          time.Time
				sats, fee                  int64
			}{u.Key, u.Account, string(u.Dir), u.Txid, u.Address, u.Time, u.Sats, u.FeeSats}
			break
		}
	}
	if it == nil {
		return "", "", errors.New("that record isn't waiting in Review")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Review item: %s %s %s %s.\n", dayOf(it.t), it.acct, it.dir, btcStr(it.sats))
	near := func(a, b int64) bool {
		d := a - b
		if d < 0 {
			d = -d
		}
		return d <= max(a/50, 20_000) // within 2% or 0.0002 BTC (fees, rounding)
	}
	type cand struct {
		line  string
		score float64
	}
	var cs []cand
	window := 5 * 24 * time.Hour
	for _, u := range tb.rep.Unmatched {
		if u.Key == it.key || u.Account == it.acct || string(u.Dir) == it.dir {
			continue
		}
		dt := u.Time.Sub(it.t)
		if dt < 0 {
			dt = -dt
		}
		if dt <= window && near(it.sats, u.Sats) {
			cs = append(cs, cand{"- Also waiting in Review: " + itemLine(u.Key, u.Time, u.Account, string(u.Dir), u.Sats, u.FeeSats, u.Value, u.Network, u.Note, u.Txid) + " (label both as a transfer between your accounts, or link them)", dt.Hours()})
		}
	}
	for _, g := range tb.rep.Graph {
		if g.Account == it.acct || g.Key == it.key {
			continue
		}
		dt := time.Unix(g.Time, 0).Sub(it.t)
		if dt < 0 {
			dt = -dt
		}
		if dt <= window && near(it.sats, g.Sats) && g.Kind != "transfer" {
			cs = append(cs, cand{"- Similar record in another account: " + graphLine(g), dt.Hours() + 1})
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].score < cs[j].score })
	if len(cs) == 0 {
		b.WriteString("No record in the user's other tracked accounts has a similar amount within 5 days, which is why it wasn't matched automatically. ")
		if it.dir == "out" {
			b.WriteString("So the coins most likely went somewhere this app doesn't track: a sale or payment to someone (a spend or a sale), a gift, or a wallet the user owns but hasn't added. If it's the user's own wallet, adding that wallet will match it automatically; otherwise label it with what it was.\n")
		} else {
			b.WriteString("So the coins most likely came from outside the tracked accounts: a purchase from a person, income, a gift received, or a wallet the user owns but hasn't added.\n")
		}
	} else {
		fmt.Fprintf(&b, "Possible other sides of a transfer (closest in time first):\n")
		for i, c := range cs {
			if i >= 10 {
				break
			}
			b.WriteString(c.line + "\n")
		}
		b.WriteString("Automatic matching needs the same txid or address, or amounts and times that agree closely; these didn't agree closely enough, so the user should confirm.\n")
	}
	return b.String(), "Looked for matches for " + recordName(tb.rep, it.key), nil
}

// --- data health ---

func (tb *toolbox) health() (string, string) {
	var b strings.Builder
	issues := 0
	add := func(format string, a ...any) {
		issues++
		fmt.Fprintf(&b, "- "+format+"\n", a...)
	}
	if n := len(tb.rep.Unmatched); n > 0 {
		add("%d items are waiting in Review; until they're labeled, balances and taxes may be off.", n)
	}
	for _, y := range tb.rep.Years {
		if y.MissingBasis > 0 {
			add("%d: %s was sold without a known cost basis (more sold than the app knows was bought); check for missing imports or wallets.", y.Year, btcStr(y.MissingBasis))
		}
	}
	for _, a := range tb.rep.Accounts {
		if a.ChainSats != nil && *a.ChainSats != a.Sats {
			add("%s: the app counts %s but the blockchain shows %s (unlabeled items or a stale scan).", a.Name, btcStr(a.Sats), btcStr(*a.ChainSats))
		}
		if a.Kind == "wallet" && a.ScannedAt != "" {
			if t, err := time.Parse(time.RFC3339, a.ScannedAt); err == nil && time.Since(t) > 30*24*time.Hour {
				add("Wallet %s was last scanned %s.", a.Name, dayOf(t))
			}
		}
	}
	if ex, err := tb.s.st.ExchangeEvents(); err == nil {
		last := map[string]time.Time{}
		for _, e := range ex {
			if e.Time.After(last[e.Account]) {
				last[e.Account] = e.Time
			}
		}
		for a, t := range last {
			if d := int(time.Since(t).Hours() / 24); d > 45 {
				add("%s: the newest imported record is from %s (%d days ago); import a fresh export.", a, dayOf(t), d)
			}
		}
	}
	for i, w := range tb.rep.Warnings {
		if i >= 10 {
			break
		}
		add("Warning: %s", w)
	}
	if issues == 0 {
		return "No problems found: nothing in Review, every sale has a cost basis, balances agree, and imports and scans are recent.", "Checked data health"
	}
	return fmt.Sprintf("%d things to look at:\n", issues) + b.String(), fmt.Sprintf("Checked data health: %d issues", issues)
}

// --- on-chain, income, Lightning ---

func (tb *toolbox) onchain(txid string) (string, string, error) {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if !txidRe.MatchString(txid) {
		return "", "", errors.New("a transaction id is 64 hex characters")
	}
	req := httptest.NewRequest("GET", "/api/tx/"+txid, nil)
	req.SetPathValue("txid", txid)
	rec := httptest.NewRecorder()
	tb.s.getTx(rec, req)
	if rec.Code != 200 {
		var e struct{ Error string }
		json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error == "" {
			e.Error = "couldn't fetch the transaction"
		}
		return "", "", errors.New(e.Error)
	}
	var out bytes.Buffer
	json.Indent(&out, rec.Body.Bytes(), "", " ")
	return "On-chain transaction (from the user's Fulcrum server):\n" + clip(out.String(), 6000), "Fetched transaction " + txid[:10] + "…", nil
}

func (tb *toolbox) income(y int) (string, string) {
	var b strings.Builder
	var sats, cents int64
	n := 0
	for _, r := range tb.rep.Income {
		if y != 0 && r.Time.UTC().Year() != y {
			continue
		}
		n++
		sats += r.Sats
		cents += r.Cents
		if n <= toolRowCap {
			fmt.Fprintf(&b, "- [%s] %s %s: %s, worth %s when received\n", r.Key, dayOf(r.Time), r.Kind, btcStr(r.Sats), usdStr(r.Cents))
		}
	}
	if n == 0 {
		return fmt.Sprintf("No income records in %d.", y), fmt.Sprintf("Checked income in %d", y)
	}
	return fmt.Sprintf("%d income records in %d, totaling %s worth %s (taxed as ordinary income when received):\n", n, y, btcStr(sats), usdStr(cents)) + b.String(), fmt.Sprintf("Looked up income in %d", y)
}

func (tb *toolbox) lightningActivity(fromS, toS string) (string, string, error) {
	ln := tb.rep.Lightning
	if ln == nil {
		return "", "", errors.New("no Lightning node is connected")
	}
	from, hasFrom := parseDay(fromS)
	to, hasTo := parseDay(toS)
	type agg struct {
		n             int
		credit, debit int64
	}
	byTag := map[string]*agg{}
	for _, e := range ln.Events {
		t := time.Unix(e.Time, 0).UTC()
		if hasFrom && t.Before(from) || hasTo && t.After(to.Add(24*time.Hour-time.Second)) {
			continue
		}
		a := byTag[e.Tag]
		if a == nil {
			a = &agg{}
			byTag[e.Tag] = a
		}
		a.n++
		a.credit += e.CreditSats
		a.debit += e.DebitSats
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lightning node %s: balance now %s, %d channels.\nActivity by type", ln.Account, btcStr(ln.Balance), len(ln.Channels))
	if hasFrom || hasTo {
		fmt.Fprintf(&b, " between %s and %s", fromS, toS)
	}
	b.WriteString(":\n")
	tags := make([]string, 0, len(byTag))
	for t := range byTag {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, t := range tags {
		a := byTag[t]
		fmt.Fprintf(&b, "- %s: %d events, in %s, out %s\n", t, a.n, btcStr(a.credit), btcStr(a.debit))
	}
	if len(tags) == 0 {
		b.WriteString("- none\n")
	}
	return b.String(), "Looked up Lightning activity", nil
}

// --- tracing ---

func (tb *toolbox) trace(id, acct string) (string, string, error) {
	byKey := map[string]GraphEvent{}
	for _, g := range tb.rep.Graph {
		byKey[g.Key] = g
	}
	origin := func(oid string) string {
		if g, ok := byKey[oid]; ok {
			return fmt.Sprintf("%s %s %s on %s", kindName(g.Kind), btcStr(g.Sats), "in "+g.Account, dayOf(time.Unix(g.Time, 0)))
		}
		return "an unknown source [" + oid + "]"
	}
	var b strings.Builder
	if acct != "" && id == "" {
		var found bool
		for _, a := range tb.rep.Accounts {
			if strings.EqualFold(a.Name, acct) {
				acct, found = a.Name, true
			}
		}
		if !found {
			return "", "", fmt.Errorf("no account called %q", acct)
		}
		type src struct{ sats, basis int64 }
		srcs := map[string]*src{}
		var order []string
		for _, l := range tb.rep.OpenLots {
			if l.Account != acct || l.Sats <= 0 {
				continue
			}
			s := srcs[l.OriginID]
			if s == nil {
				s = &src{}
				srcs[l.OriginID] = s
				order = append(order, l.OriginID)
			}
			s.sats += l.Sats
			s.basis += l.Basis
		}
		sort.Slice(order, func(i, j int) bool { return srcs[order[i]].sats > srcs[order[j]].sats })
		fmt.Fprintf(&b, "The coins %s holds now came from %d sources, largest first:\n", acct, len(order))
		for i, o := range order {
			if i >= toolRowCap {
				fmt.Fprintf(&b, "(%d more)\n", len(order)-toolRowCap)
				break
			}
			fmt.Fprintf(&b, "- %s from %s, cost basis %s\n", btcStr(srcs[o].sats), origin(o), usdStr(srcs[o].basis))
		}
		return b.String(), "Traced the coins in " + acct, nil
	}
	id = strings.Trim(strings.TrimSpace(id), "[]`")
	var ev *GraphEvent
	for i := range tb.rep.Graph {
		if sameRecord(tb.rep.Graph[i].Key, tb.rep.Graph[i].Txid, id) {
			ev = &tb.rep.Graph[i]
			break
		}
	}
	if ev == nil {
		return "", "", errors.New("no record with that id or txid; search for it first")
	}
	fmt.Fprintf(&b, "Record: %s\n", graphLine(*ev))
	// Forward: where coins from this acquisition ended up.
	var forward bool
	var sold, removed, held int64
	var fl []string
	for _, d := range tb.rep.Disposals {
		if d.OriginID == ev.Key {
			forward = true
			sold += d.Sats
			term := "short-term"
			if d.LongTerm {
				term = "long-term"
			}
			fl = append(fl, fmt.Sprintf("- %s sold or spent from %s on %s: proceeds %s, basis %s, gain %s (%s)", btcStr(d.Sats), d.Account, dayOf(d.Disposed), usdStr(d.Proceeds), usdStr(d.Basis), usdStr(d.Gain()), term))
		}
	}
	for _, r := range tb.rep.Removals {
		if r.OriginID == ev.Key {
			forward = true
			removed += r.Sats
			fl = append(fl, fmt.Sprintf("- %s left %s on %s as a gift, donation or loss (basis %s)", btcStr(r.Sats), r.Account, dayOf(r.Removed), usdStr(r.Basis)))
		}
	}
	for _, l := range tb.rep.OpenLots {
		if l.OriginID == ev.Key && l.Sats > 0 {
			forward = true
			held += l.Sats
			where := l.Account
			if where == "untracked" {
				where = "a wallet not added to the app"
			}
			fl = append(fl, fmt.Sprintf("- %s still held in %s (basis %s)", btcStr(l.Sats), where, usdStr(l.Basis)))
		}
	}
	if forward {
		fmt.Fprintf(&b, "Where these coins went: %s sold or spent, %s given away or lost, %s still held.\n", btcStr(sold), btcStr(removed), btcStr(held))
		for i, l := range fl {
			if i >= toolRowCap {
				break
			}
			b.WriteString(l + "\n")
		}
	}
	// Backward: where the coins of this sale/spend/gift came from.
	var back []string
	type srcSum struct {
		sats, basis int64
		first, last time.Time
		n           int
	}
	bySrc := map[string]*srcSum{}
	var srcOrder []string
	note := func(oid string, sats, basis int64, acq time.Time) {
		where := "an unknown source"
		if g, ok := byKey[oid]; ok {
			where = kindName(g.Kind) + " in " + g.Account
		}
		x := bySrc[where]
		if x == nil {
			x = &srcSum{first: acq, last: acq}
			bySrc[where] = x
			srcOrder = append(srcOrder, where)
		}
		x.sats += sats
		x.basis += basis
		x.n++
		if acq.Before(x.first) {
			x.first = acq
		}
		if acq.After(x.last) {
			x.last = acq
		}
	}
	for _, d := range tb.rep.Disposals {
		if d.OpID == ev.Key || strings.HasPrefix(d.OpID, ev.Key+":") || strings.HasPrefix(d.OpID, ev.Key+">") {
			back = append(back, fmt.Sprintf("- %s from %s (acquired %s, basis %s)%s", btcStr(d.Sats), origin(d.OriginID), dayOf(d.Acquired), usdStr(d.Basis), tb.route(d.OriginID, d.Account, d.Disposed)))
			note(d.OriginID, d.Sats, d.Basis, d.Acquired)
		}
	}
	for _, r := range tb.rep.Removals {
		if r.OpID == ev.Key {
			back = append(back, fmt.Sprintf("- %s from %s (acquired %s, basis %s)%s", btcStr(r.Sats), origin(r.OriginID), dayOf(r.Acquired), usdStr(r.Basis), tb.route(r.OriginID, r.Account, r.Removed)))
			note(r.OriginID, r.Sats, r.Basis, r.Acquired)
		}
	}
	if len(back) > 0 {
		sort.Slice(srcOrder, func(i, j int) bool { return bySrc[srcOrder[i]].sats > bySrc[srcOrder[j]].sats })
		fmt.Fprintf(&b, "Where the coins in this record came from (the %s method picked these). Summary by source, lead with this:\n", strings.ToUpper(tb.rep.Settings["method"]))
		for _, k := range srcOrder {
			x := bySrc[k]
			span := dayOf(x.first)
			if !x.last.Equal(x.first) {
				span += " to " + dayOf(x.last)
			}
			fmt.Fprintf(&b, "- %s: %s from %d lots acquired %s, cost basis %s\n", k, btcStr(x.sats), x.n, span, usdStr(x.basis))
		}
		b.WriteString("Each lot (only list these if the user asks for detail):\n")
		for i, l := range back {
			if i >= toolRowCap {
				fmt.Fprintf(&b, "(%d more lots)\n", len(back)-toolRowCap)
				break
			}
			b.WriteString(l + "\n")
		}
	}
	if ev.Kind == "transfer" && ev.To != "" {
		fmt.Fprintf(&b, "This record moved coins from %s to %s; it isn't taxable.\n", ev.Account, ev.To)
	}
	if !forward && len(back) == 0 && ev.Kind != "transfer" {
		b.WriteString("No lots are tied to this record (for example it's unlabeled, or a transfer).\n")
	}
	return b.String(), "Traced " + recordName(tb.rep, ev.Key), nil
}

// route finds the likely path of transfers from where coins were acquired
// to the account they left from, between the two dates.
func (tb *toolbox) route(originID, dest string, by time.Time) string {
	var start string
	var since int64
	for _, g := range tb.rep.Graph {
		if g.Key == originID {
			start, since = g.Account, g.Time
		}
	}
	if start == "" || start == dest {
		return ""
	}
	type hop struct {
		acct string
		t    int64
		path []GraphEvent
	}
	var transfers []GraphEvent
	for _, g := range tb.rep.Graph {
		if g.Kind == "transfer" && g.To != "" && g.Time >= since && g.Time <= by.Unix() {
			transfers = append(transfers, g)
		}
	}
	sort.Slice(transfers, func(i, j int) bool { return transfers[i].Time < transfers[j].Time })
	// Earliest-arrival search over time-ordered transfers.
	best := map[string]hop{start: {start, since, nil}}
	for _, tr := range transfers {
		from, ok := best[tr.Account]
		if !ok || from.t > tr.Time {
			continue
		}
		if cur, seen := best[tr.To]; !seen || tr.Time < cur.t {
			best[tr.To] = hop{tr.To, tr.Time, append(append([]GraphEvent(nil), from.path...), tr)}
		}
	}
	h, ok := best[dest]
	if !ok || len(h.path) == 0 {
		return ", route between accounts unknown"
	}
	var parts []string
	for _, p := range h.path {
		parts = append(parts, fmt.Sprintf("%s to %s on %s", p.Account, p.To, dayOf(time.Unix(p.Time, 0))))
	}
	return ", likely moved " + strings.Join(parts, ", then ")
}

// --- what-if with chosen coins ---

func (tb *toolbox) whatIfPicks(a map[string]any) (string, string, error) {
	in := map[string]any{"account": argStr(a, "account"), "btc": argNum(a, "btc"), "usd": argNum(a, "usd"), "strategy": strings.ToLower(argStr(a, "strategy"))}
	if l, ok := a["lots"].([]any); ok && len(l) > 0 {
		var ids []string
		for _, x := range l {
			if s, ok := x.(string); ok {
				ids = append(ids, s)
			}
		}
		in["lots"] = ids
		delete(in, "strategy")
	}
	out, _, err := tb.whatIf(in)
	if err != nil {
		return "", "", err
	}
	how := "chosen lots"
	if s, _ := in["strategy"].(string); s != "" {
		how = strings.ReplaceAll(s, "_", " ") + " first"
	}
	return out, "Estimated a sale from " + argStr(a, "account") + " (" + how + ")", nil
}

// resolveAccount matches an account name the way a model writes it:
// any case, with a type tacked on ("Cold storage (wallet)"), or a unique
// start of the name.
func (tb *toolbox) resolveAccount(name string) (string, error) {
	name = strings.TrimSpace(name)
	var names []string
	for _, a := range tb.rep.Accounts {
		names = append(names, a.Name)
	}
	try := func(n string) (string, bool) {
		for _, x := range names {
			if strings.EqualFold(x, n) {
				return x, true
			}
		}
		return "", false
	}
	for n := name; n != ""; {
		if x, ok := try(n); ok {
			return x, nil
		}
		i := strings.LastIndex(n, " (")
		if i < 0 || !strings.HasSuffix(n, ")") {
			break
		}
		n = strings.TrimSpace(n[:i])
	}
	var hits []string
	for _, x := range names {
		if name != "" && strings.HasPrefix(strings.ToLower(x), strings.ToLower(name)) {
			hits = append(hits, x)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	return "", fmt.Errorf("no account called %q; the accounts are: %s", name, strings.Join(names, ", "))
}

// sellEverything estimates selling every coin in every tracked account
// today, all at once, so the year's tax is worked out on the total.
func (tb *toolbox) sellEverything() (string, string, error) {
	p, err := tb.s.pipeline()
	if err != nil {
		return "", "", err
	}
	day, price, ok := p.prices.latest()
	if !ok || price <= 0 {
		return "", "", errors.New("no BTC price yet: update prices in Settings")
	}
	base, err := lots.Run(p.tr.Ops, p.cfg)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	ops := append([]lots.Op(nil), p.tr.Ops...)
	var accts []string
	for a, sats := range base.Balances {
		if sats > 0 && a != trace.UntrackedAccount {
			accts = append(accts, a)
		}
	}
	if len(accts) == 0 {
		return "No bitcoin is held in any tracked account, so there's nothing to sell.", "Estimated selling everything", nil
	}
	sort.Strings(accts)
	for _, a := range accts {
		sats := base.Balances[a]
		ops = append(ops, lots.Op{ID: "sellall:" + a, Kind: lots.Dispose, Time: now, Account: a, Sats: sats, Cents: lots.MulDiv(price, sats, 100_000_000)})
	}
	res, err := lots.Run(ops, p.cfg)
	if err != nil {
		return "", "", err
	}
	var yst, ylt int64 // the rest of this year, for the extra tax
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
	type side struct{ sats, proceeds, basis int64 }
	per := map[string]*[2]side{}
	var short, long side
	var missing int64
	for _, d := range res.Disposals {
		a, ok := strings.CutPrefix(d.OpID, "sellall:")
		if !ok {
			continue
		}
		if per[a] == nil {
			per[a] = &[2]side{}
		}
		i, t := 0, &short
		if d.LongTerm {
			i, t = 1, &long
		}
		for _, x := range []*side{&per[a][i], t} {
			x.sats += d.Sats
			x.proceeds += d.Proceeds
			x.basis += d.Basis
		}
		if d.Missing {
			missing += d.Sats
		}
	}
	sbp, lbp := rateBP(p.set, "rateShort"), rateBP(p.set, "rateLong")
	before := yearTax(yst, ylt, sbp, lbp)
	after := yearTax(yst+short.proceeds-short.basis, ylt+long.proceeds-long.basis, sbp, lbp)
	var b strings.Builder
	fmt.Fprintf(&b, "Selling everything today (%s) at %s per BTC (%s): %s from %d accounts.\n", now.Format("2006-01-02"), usdStr(price), day, btcStr(short.sats+long.sats), len(accts))
	fmt.Fprintf(&b, "Total proceeds %s, cost basis %s, total gain %s.\n", usdStr(short.proceeds+long.proceeds), usdStr(short.basis+long.basis), usdStr(short.proceeds+long.proceeds-short.basis-long.basis))
	fmt.Fprintf(&b, "Short-term (held a year or less): %s, gain %s. Long-term: %s, gain %s.\n", btcStr(short.sats), usdStr(short.proceeds-short.basis), btcStr(long.sats), usdStr(long.proceeds-long.basis))
	fmt.Fprintf(&b, "Estimated tax for %d goes from %s to %s, so selling everything adds %s (at the rates in Settings: short-term %s%%, long-term %s%%).\n", now.Year(), usdStr(before), usdStr(after), usdStr(after-before), p.set["rateShort"], p.set["rateLong"])
	if missing > 0 {
		fmt.Fprintf(&b, "%s would have no known cost basis (counted as all gain).\n", btcStr(missing))
	}
	if u := base.Balances[trace.UntrackedAccount]; u > 0 {
		fmt.Fprintf(&b, "Not included: %s in wallets not added to the app.\n", btcStr(u))
	}
	b.WriteString("By account:\n")
	for _, a := range accts {
		x := per[a]
		if x == nil {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s, proceeds %s, gain %s (short-term %s, long-term %s)\n", a, btcStr(x[0].sats+x[1].sats), usdStr(x[0].proceeds+x[1].proceeds),
			usdStr(x[0].proceeds+x[1].proceeds-x[0].basis-x[1].basis), usdStr(x[0].proceeds-x[0].basis), usdStr(x[1].proceeds-x[1].basis))
	}
	return b.String(), "Estimated selling everything", nil
}
