package app

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"btctax/internal/importers"
	"btctax/internal/lots"
	"btctax/internal/tax"
	"btctax/internal/trace"
)

type YearSummary struct {
	Year          int   `json:"year"`
	ShortProceeds int64 `json:"shortProceeds"`
	ShortBasis    int64 `json:"shortBasis"`
	LongProceeds  int64 `json:"longProceeds"`
	LongBasis     int64 `json:"longBasis"`
	Income        int64 `json:"income"`
	MissingBasis  int64 `json:"missingBasisSats"`
	EstimatedTax  int64 `json:"estimatedTax"`
}

type AccountView struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Sats      int64  `json:"sats"`      // per the lot engine
	ChainSats *int64 `json:"chainSats"` // per the wallet scan, wallets only
	Basis     int64  `json:"basis"`
	ScannedAt string `json:"scannedAt,omitempty"`
	Script    string `json:"script,omitempty"`
	ID        int64  `json:"id"`
}

type GraphEvent struct {
	Key     string `json:"key"`
	Account string `json:"account"`
	Time    int64  `json:"t"`
	Kind    string `json:"kind"` // buy sell income transfer unmatched, or a label category
	Sats    int64  `json:"sats"`
	To      string `json:"to,omitempty"`
	ToKey   string `json:"toKey,omitempty"`
	// Details for the transaction view.
	Cents    int64  `json:"cents,omitempty"` // USD paid, received or labeled
	FeeCents int64  `json:"feeCents,omitempty"`
	FeeSats  int64  `json:"feeSats,omitempty"`
	Txid     string `json:"txid,omitempty"`
	Address  string `json:"address,omitempty"`
	Note     string `json:"note,omitempty"`
	Method   string `json:"method,omitempty"` // how a transfer was matched
	Inputs   int    `json:"inputs,omitempty"` // on-chain shape, when known
	Outputs  int    `json:"outputs,omitempty"`
}

type Report struct {
	Accounts   []AccountView     `json:"accounts"`
	Years      []YearSummary     `json:"years"`
	Disposals  []lots.Disposal   `json:"disposals"`
	OpenLots   []lots.Lot        `json:"openLots"`
	Removals   []lots.Removal    `json:"removals"`
	Income     []trace.IncomeRow `json:"income"`
	Unmatched  []trace.Item      `json:"unmatched"`
	Links      []trace.Link      `json:"links"`
	Graph      []GraphEvent      `json:"graph"`
	Warnings   []string          `json:"warnings"`
	Price      int64             `json:"price"`
	PriceDay   string            `json:"priceDay"`
	Unrealized int64             `json:"unrealized"`
	Holdings   int64             `json:"holdingsSats"`
	Settings   map[string]string `json:"settings"`
	History    []HistoryPoint    `json:"history"`
}

// HistoryPoint is the BTC held at the end of a week and that day's price.
type HistoryPoint struct {
	T     int64 `json:"t"`
	Sats  int64 `json:"sats"`
	Price int64 `json:"price"` // USD cents per BTC, 0 if unknown
}

// history replays the engine's inputs to get total BTC held over time,
// sampled weekly from the first record to today. Coins parked in wallets
// that are not added here still count as held.
func history(ops []lots.Op, price func(time.Time) (int64, bool)) []HistoryPoint {
	if len(ops) == 0 {
		return []HistoryPoint{}
	}
	sorted := append([]lots.Op(nil), ops...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	delta := func(o lots.Op) int64 {
		switch o.Kind {
		case lots.Acquire:
			return o.Sats
		case lots.Dispose, lots.Remove:
			return -o.Sats
		case lots.Transfer:
			return -o.FeeSats
		}
		return 0
	}
	start := sorted[0].Time.UTC().Truncate(24 * time.Hour)
	end := time.Now().UTC()
	var out []HistoryPoint
	var held int64
	i := 0
	for t := start.AddDate(0, 0, 7); ; t = t.AddDate(0, 0, 7) {
		if t.After(end) {
			t = end
		}
		for i < len(sorted) && !sorted[i].Time.After(t) {
			held += delta(sorted[i])
			i++
		}
		p, _ := price(t)
		out = append(out, HistoryPoint{T: t.Unix(), Sats: held, Price: p})
		if !t.Before(end) {
			break
		}
	}
	return out
}

func (s *Server) buildReport() (*Report, error) {
	ex, err := s.st.ExchangeEvents()
	if err != nil {
		return nil, err
	}
	wal, err := s.st.WalletTxs()
	if err != nil {
		return nil, err
	}
	labels, err := s.st.Labels()
	if err != nil {
		return nil, err
	}
	prices, err := s.priceTable(ex)
	if err != nil {
		return nil, err
	}
	tr := trace.Build(ex, wal, labels, trace.Options{Price: prices.lookup})
	set := s.settings()
	rules, _ := tax.ByID(set["jurisdiction"])
	cfg := lots.Config{
		Rules:   rules,
		FeeMode: lots.FeeMode(set["feeMode"]),
		Method: func(year int) tax.Method {
			if m := s.st.Setting("method:"+strconv.Itoa(year), ""); m != "" {
				return tax.Method(m)
			}
			return tax.Method(set["method"])
		},
	}
	res, err := lots.Run(tr.Ops, cfg)
	if err != nil {
		return nil, err
	}
	r := &Report{Disposals: res.Disposals, OpenLots: res.Open, Removals: res.Removals, Income: tr.Income, Unmatched: tr.Unmatched,
		Links: tr.Links, Warnings: tr.Warnings, Settings: set}
	for _, w := range res.Warnings {
		r.Warnings = append(r.Warnings, w.OpID+": "+w.Message)
	}

	// Per-year summary.
	years := map[int]*YearSummary{}
	y := func(t time.Time) *YearSummary {
		if years[t.Year()] == nil {
			years[t.Year()] = &YearSummary{Year: t.Year()}
		}
		return years[t.Year()]
	}
	for _, d := range res.Disposals {
		ys := y(d.Disposed)
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
	for _, in := range tr.Income {
		y(in.Time).Income += in.Cents
	}
	rate := func(k string) int64 { v, _ := strconv.ParseFloat(set[k], 64); return int64(v * 100) } // basis points
	for _, ys := range years {
		st := ys.ShortProceeds - ys.ShortBasis + ys.Income
		lt := ys.LongProceeds - ys.LongBasis
		// Net losses offset gains; the US $3,000 ordinary offset and
		// carryforwards are not modelled yet.
		if st+lt > 0 {
			if lt < 0 {
				st += lt
				lt = 0
			} else if st < 0 {
				lt += st
				st = 0
			}
			ys.EstimatedTax = lots.MulDiv(max(st, 0), rate("rateShort"), 10000) + lots.MulDiv(max(lt, 0), rate("rateLong"), 10000)
		}
		r.Years = append(r.Years, *ys)
	}
	sort.Slice(r.Years, func(i, j int) bool { return r.Years[i].Year < r.Years[j].Year })

	// Accounts: engine balance and basis, plus chain balance for wallets.
	accts, err := s.st.Accounts()
	if err != nil {
		return nil, err
	}
	basis := map[string]int64{}
	for _, l := range res.Open {
		basis[l.Account] += l.Basis
		r.Holdings += l.Sats
	}
	seen := map[string]bool{}
	for _, a := range accts {
		v := AccountView{ID: a.ID, Name: a.Name, Kind: a.Kind, Sats: res.Balances[a.Name], Basis: basis[a.Name], ScannedAt: a.ScannedAt}
		if a.Kind == "wallet" {
			b := a.Balance
			v.ChainSats = &b
			v.Script = a.Script
			if v.Script == "" {
				v.Script = "descriptor"
			}
		}
		r.Accounts = append(r.Accounts, v)
		seen[a.Name] = true
	}
	for name, sats := range res.Balances {
		if !seen[name] && (sats != 0 || basis[name] != 0) {
			r.Accounts = append(r.Accounts, AccountView{Name: name, Kind: "other", Sats: sats, Basis: basis[name]})
		}
	}
	if len(res.Open) > 0 && basis[""] != 0 {
		r.Accounts = append(r.Accounts, AccountView{Name: "(pooled before per-wallet tracking)", Kind: "pool", Basis: basis[""]})
	}

	var totalBasis int64
	for _, b := range basis {
		totalBasis += b
	}
	if day, p, ok := prices.latest(); ok {
		r.Price, r.PriceDay = p, day
		r.Unrealized = lots.MulDiv(p, r.Holdings, 100_000_000) - totalBasis
	}
	r.Graph = graph(ex, tr, labels)
	r.History = history(tr.Ops, prices.lookup)
	r.fillNil()
	return r, nil
}

// fillNil turns empty lists into [] rather than null, so the UI can treat
// every list the same way.
func (r *Report) fillNil() {
	if r.Accounts == nil {
		r.Accounts = []AccountView{}
	}
	if r.Years == nil {
		r.Years = []YearSummary{}
	}
	if r.Disposals == nil {
		r.Disposals = []lots.Disposal{}
	}
	if r.OpenLots == nil {
		r.OpenLots = []lots.Lot{}
	}
	if r.Removals == nil {
		r.Removals = []lots.Removal{}
	}
	if r.Income == nil {
		r.Income = []trace.IncomeRow{}
	}
	if r.Unmatched == nil {
		r.Unmatched = []trace.Item{}
	}
	if r.Links == nil {
		r.Links = []trace.Link{}
	}
	if r.Graph == nil {
		r.Graph = []GraphEvent{}
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	if r.History == nil {
		r.History = []HistoryPoint{}
	}
}

func graph(ex []trace.ExEvent, tr *trace.Result, labels map[string]trace.Label) []GraphEvent {
	var g []GraphEvent
	for _, e := range ex {
		var k string
		switch e.Kind {
		case importers.Buy:
			k = "buy"
		case importers.Sell:
			k = "sell"
		case importers.Income, importers.Reward:
			k = "income"
		default:
			continue
		}
		g = append(g, GraphEvent{Key: "ex:" + e.ID, Account: e.Account, Time: e.Time.Unix(), Kind: k, Sats: e.Sats,
			Cents: e.FiatCents, FeeCents: e.FeeCents, Txid: e.Txid, Address: e.Address, Note: e.Note})
	}
	for _, l := range tr.Links {
		f, t := tr.Items[l.From], tr.Items[l.To]
		txid := f.Txid
		if txid == "" {
			txid = t.Txid
		}
		addr := t.Address
		if addr == "" {
			addr = f.Address
		}
		fee := f.Sats + f.FeeSats - t.Sats
		if f.FeeSats > 0 && t.Sats == f.Sats-f.FeeSats {
			fee = f.FeeSats
		}
		g = append(g, GraphEvent{Key: l.From, Account: f.Account, Time: f.Time.Unix(), Kind: "transfer", Sats: t.Sats, To: t.Account, ToKey: l.To,
			FeeSats: max(fee, 0), Txid: txid, Address: addr, Note: f.Note, Method: l.Method})
	}
	for _, op := range tr.Ops {
		if op.Kind == lots.Transfer && op.Account == op.To && strings.HasSuffix(op.ID, ":fee") {
			// Coins merged inside one wallet: only the fee left.
			txid := strings.Split(op.ID, ":")[1]
			g = append(g, GraphEvent{Key: op.ID, Account: op.Account, Time: op.Time.Unix(), Kind: "consolidation",
				Sats: tr.Txs[txid].OutSats, FeeSats: op.FeeSats, Txid: txid})
			continue
		}
		if op.Kind == lots.Transfer && op.Account != op.To && tr.Items[op.ID].Key == "" && op.To != "" && !containsGT(op.ID) {
			ev := GraphEvent{Key: op.ID, Account: op.Account, Time: op.Time.Unix(), Kind: "transfer", Sats: op.Sats, To: op.To,
				FeeSats: op.FeeSats, Method: "same transaction"}
			if p := strings.Split(op.ID, ":"); len(p) >= 3 && p[0] == "tx" {
				ev.Txid = p[1]
			}
			g = append(g, ev)
		}
	}
	item := func(it trace.Item, kind string) GraphEvent {
		return GraphEvent{Key: it.Key, Account: it.Account, Time: it.Time.Unix(), Kind: kind, Sats: it.Sats,
			FeeSats: it.FeeSats, Txid: it.Txid, Address: it.Address, Note: it.Note}
	}
	unmatched := map[string]bool{}
	for _, it := range tr.Unmatched {
		unmatched[it.Key] = true
		g = append(g, item(it, "unmatched"))
	}
	for k, l := range labels {
		it, ok := tr.Items[k]
		if !ok || unmatched[k] || l.Category == trace.SelfLink {
			continue
		}
		ev := item(it, string(l.Category))
		ev.Cents = l.FiatCents
		if l.Note != "" {
			ev.Note = l.Note
		}
		if l.Category == trace.Untracked {
			if it.Dir == trace.Out {
				ev.To = "untracked"
			}
		}
		g = append(g, ev)
	}
	for i := range g {
		if info, ok := tr.Txs[g[i].Txid]; ok {
			g[i].Inputs, g[i].Outputs = info.Inputs, info.Outputs
		}
	}
	sort.Slice(g, func(i, j int) bool { return g[i].Time < g[j].Time })
	return g
}

func containsGT(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '>' {
			return true
		}
	}
	return false
}

// prices combines imported daily prices with prices implied by exchange
// trades. Nothing is fetched from the internet.
type priceTable struct{ days map[string]int64 }

func (s *Server) priceTable(ex []trace.ExEvent) (*priceTable, error) {
	imported, err := s.st.Prices()
	if err != nil {
		return nil, err
	}
	type acc struct{ cents, sats int64 }
	implied := map[string]*acc{}
	for _, e := range ex {
		if (e.Kind == importers.Buy || e.Kind == importers.Sell) && e.Sats > 0 && e.FiatCents > 0 {
			d := e.Time.Format("2006-01-02")
			if implied[d] == nil {
				implied[d] = &acc{}
			}
			implied[d].cents += e.FiatCents
			implied[d].sats += e.Sats
		}
	}
	// Lowest priority first: prices implied by the user's own trades, then
	// the history baked into the binary, then imported or fetched prices.
	p := &priceTable{days: map[string]int64{}}
	for d, a := range implied {
		p.days[d] = lots.MulDiv(a.cents, 100_000_000, a.sats)
	}
	for d, c := range s.bundled {
		p.days[d] = c
	}
	for d, c := range imported {
		p.days[d] = c
	}
	return p, nil
}

// lookup returns the price on the day, or the closest earlier day within a week.
func (p *priceTable) lookup(t time.Time) (int64, bool) {
	for i := 0; i < 7; i++ {
		if c, ok := p.days[t.AddDate(0, 0, -i).Format("2006-01-02")]; ok {
			return c, true
		}
	}
	return 0, false
}

func (p *priceTable) latest() (string, int64, bool) {
	best := ""
	for d := range p.days {
		if d > best {
			best = d
		}
	}
	return best, p.days[best], best != ""
}
