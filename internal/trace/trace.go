// Package trace links exchange records to on-chain wallet activity,
// builds the labeling queue for what it cannot prove, and turns the result
// into operations for the lot engine.
package trace

import (
	"fmt"
	"sort"
	"time"

	"btctax/internal/importers"
	"btctax/internal/lots"
	"btctax/internal/scan"
)

// ExEvent is an imported exchange row with its storage id and account.
type ExEvent struct {
	ID      string
	Account string
	importers.Event
}

type WalletTxs struct {
	Account string
	Txs     []scan.Tx
}

type Dir string

const (
	In  Dir = "in"
	Out Dir = "out"
)

// Item is anything that moves BTC into or out of the tracked set and may
// need linking or a label.
type Item struct {
	Key     string // "ex:<id>" or "tx:<txid>:<vout>"
	Source  string // "exchange" or "chain"
	Account string
	Dir     Dir
	Time    time.Time
	Sats    int64
	FeeSats int64
	Address string
	Txid    string
	Kind    string // importer kind for exchange items
	Note    string
}

type Link struct {
	From, To string // item keys; From is the outflow
	Method   string // txid | address | amount | internal | manual
}

// Category is what the user says an unmatched item was.
type Category string

const (
	// outflows
	Spend    Category = "spend"    // paid for goods/services: disposal at FMV
	Sold     Category = "sold"     // sold peer-to-peer: disposal at given proceeds
	Gift     Category = "gift"     // gift sent: no gain, basis leaves
	Donation Category = "donation" // charitable donation: no gain, basis leaves
	Lost     Category = "lost"     // lost or stolen
	// inflows
	IncomeIn   Category = "income"   // payment for work, mining: ordinary income at FMV
	GiftIn     Category = "gift-in"  // gift received: carryover basis and date
	PurchaseIn Category = "purchase" // bought peer-to-peer
	RefundIn   Category = "refund"   // refund of an earlier spend
	// either direction
	Untracked Category = "untracked" // to/from a wallet of mine that is not tracked
	SelfLink  Category = "self"      // the other side is item Counterpart
)

type Label struct {
	Key         string
	Category    Category
	Counterpart string    // for SelfLink
	FiatCents   int64     // FMV, proceeds or basis depending on category
	AcquiredAt  time.Time // GiftIn: donor's acquisition date
	Note        string
}

// PriceFunc returns the USD price in cents for 1 BTC on the given day.
type PriceFunc func(day time.Time) (int64, bool)

type IncomeRow struct {
	Key   string
	Time  time.Time
	Sats  int64
	Cents int64
	Kind  string
}

type Result struct {
	Items     map[string]Item
	Links     []Link
	Unmatched []Item
	Ops       []lots.Op
	Income    []IncomeRow
	Warnings  []string
}

type Options struct {
	// Window is how far apart a withdrawal and its on-chain receipt may be.
	Window time.Duration
	Price  PriceFunc
}

func Build(ex []ExEvent, wallets []WalletTxs, labels map[string]Label, opt Options) *Result {
	if opt.Window == 0 {
		opt.Window = 72 * time.Hour
	}
	if opt.Price == nil {
		opt.Price = func(time.Time) (int64, bool) { return 0, false }
	}
	b := &builder{res: &Result{Items: map[string]Item{}}, opt: opt, linked: map[string]bool{}}
	b.chainLegs(wallets)
	var withdrawals, deposits []string
	for _, e := range ex {
		key := "ex:" + e.ID
		switch e.Kind {
		case importers.Buy:
			b.op(lots.Op{ID: key, Kind: lots.Acquire, Time: e.Time, Account: e.Account, Sats: e.Sats, Cents: e.FiatCents})
		case importers.Sell:
			b.op(lots.Op{ID: key, Kind: lots.Dispose, Time: e.Time, Account: e.Account, Sats: e.Sats, Cents: e.FiatCents, FeeCents: e.FeeCents})
		case importers.Income, importers.Reward:
			b.income(key, e.Account, e.Time, e.Sats, e.FiatCents, string(e.Kind))
		case importers.Withdrawal, importers.Deposit:
			it := Item{Key: key, Source: "exchange", Account: e.Account, Time: e.Time, Sats: e.Sats,
				FeeSats: e.FeeSats, Address: e.Address, Txid: e.Txid, Kind: string(e.Kind), Note: e.Note, Dir: In}
			if e.Kind == importers.Withdrawal {
				it.Dir = Out
				withdrawals = append(withdrawals, key)
			} else {
				deposits = append(deposits, key)
			}
			b.res.Items[key] = it
		}
	}
	// Manual links first: the user's word beats any heuristic.
	for _, l := range labels {
		if l.Category == SelfLink && l.Counterpart != "" {
			b.link(l.Key, l.Counterpart, "manual")
		}
	}
	ins, outs := b.chainKeys(In), b.chainKeys(Out)
	// exchange withdrawal -> wallet receipt; wallet send -> exchange deposit
	b.match(withdrawals, ins, false)
	b.match(outs, deposits, false)
	// exchange -> exchange directly
	b.match(withdrawals, deposits, true)

	for _, k := range b.sortedItemKeys() {
		if b.linked[k] {
			continue
		}
		it := b.res.Items[k]
		if l, ok := labels[k]; ok && l.Category != SelfLink {
			b.applyLabel(it, l)
			continue
		}
		b.res.Unmatched = append(b.res.Unmatched, it)
	}
	return b.res
}

type builder struct {
	res    *Result
	opt    Options
	linked map[string]bool
}

func (b *builder) op(o lots.Op) { b.res.Ops = append(b.res.Ops, o) }

func (b *builder) warn(f string, a ...any) {
	b.res.Warnings = append(b.res.Warnings, fmt.Sprintf(f, a...))
}

func (b *builder) fmv(key string, t time.Time, sats, given int64) int64 {
	if given > 0 {
		return given
	}
	if p, ok := b.opt.Price(t); ok {
		return mulDiv(p, sats, 100_000_000)
	}
	b.warn("%s: no price for %s; fair market value set to 0 until a price is imported or entered", key, t.Format("2006-01-02"))
	return 0
}

func (b *builder) income(key, account string, t time.Time, sats, given int64, kind string) {
	v := b.fmv(key, t, sats, given)
	b.op(lots.Op{ID: key, Kind: lots.Acquire, Time: t, Account: account, Sats: sats, Cents: v})
	b.res.Income = append(b.res.Income, IncomeRow{key, t, sats, v, kind})
}

// chainLegs splits every wallet transaction into internal transfers
// (linked immediately) and legs that cross the boundary of tracked wallets.
func (b *builder) chainLegs(wallets []WalletTxs) {
	type agg struct {
		tx     scan.Tx
		sent   map[string]int64 // account -> value of its inputs
		owner  map[uint32]string
		fee    int64
		feeSet bool
	}
	txs := map[string]*agg{}
	var order []string
	for _, w := range wallets {
		for _, t := range w.Txs {
			if t.Height <= 0 {
				continue // unconfirmed: wait for a block time
			}
			a, ok := txs[t.Txid]
			if !ok {
				a = &agg{tx: t, sent: map[string]int64{}, owner: map[uint32]string{}}
				txs[t.Txid] = a
				order = append(order, t.Txid)
			}
			if t.SentSats > 0 {
				a.sent[w.Account] += t.SentSats
			}
			for _, o := range t.Outputs {
				if o.Ours {
					a.owner[o.Vout] = w.Account
				}
			}
			if t.FeeSats > 0 {
				a.fee, a.feeSet = t.FeeSats, true
			}
		}
	}
	for _, id := range order {
		a := txs[id]
		t := a.tx
		key := func(v uint32) string { return fmt.Sprintf("tx:%s:%d", id, v) }
		if len(a.sent) == 0 {
			for _, o := range t.Outputs {
				if acct, ok := a.owner[o.Vout]; ok {
					b.res.Items[key(o.Vout)] = Item{Key: key(o.Vout), Source: "chain", Account: acct, Dir: In,
						Time: t.Time, Sats: o.Sats, Address: o.Address, Txid: id}
				}
			}
			continue
		}
		sender := ""
		for acct, v := range a.sent {
			if sender == "" || v > a.sent[sender] || (v == a.sent[sender] && acct < sender) {
				sender = acct
			}
		}
		if len(a.sent) > 1 {
			b.warn("tx %s spends from more than one tracked wallet; treated as sent from %s", id, sender)
		}
		if !a.feeSet {
			b.warn("tx %s has inputs from outside the tracked wallets (coinjoin or payjoin?); review it", id)
		}
		fee := a.fee
		var internal []lots.Op
		for _, o := range t.Outputs {
			acct, ours := a.owner[o.Vout]
			switch {
			case ours && acct == sender:
				// change
			case ours:
				internal = append(internal, lots.Op{ID: key(o.Vout), Kind: lots.Transfer, Time: t.Time,
					Account: sender, To: acct, Sats: o.Sats})
			default:
				it := Item{Key: key(o.Vout), Source: "chain", Account: sender, Dir: Out, Time: t.Time,
					Sats: o.Sats, Address: o.Address, Txid: id}
				if fee > 0 { // the first external output carries the whole fee
					it.FeeSats, fee = fee, 0
				}
				b.res.Items[it.Key] = it
			}
		}
		if fee > 0 && len(internal) > 0 {
			internal[0].FeeSats, fee = fee, 0
		}
		for _, o := range internal {
			b.op(o)
		}
		if fee > 0 { // consolidation inside one wallet: only the fee leaves
			b.op(lots.Op{ID: key(0) + ":fee", Kind: lots.Transfer, Time: t.Time, Account: sender, To: sender, FeeSats: fee})
		}
	}
}

func (b *builder) chainKeys(d Dir) []string {
	var out []string
	for _, k := range b.sortedItemKeys() {
		if it := b.res.Items[k]; it.Source == "chain" && it.Dir == d {
			out = append(out, k)
		}
	}
	return out
}

func (b *builder) sortedItemKeys() []string {
	keys := make([]string, 0, len(b.res.Items))
	for k := range b.res.Items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, c := b.res.Items[keys[i]], b.res.Items[keys[j]]
		if !a.Time.Equal(c.Time) {
			return a.Time.Before(c.Time)
		}
		return keys[i] < keys[j]
	})
	return keys
}

// match links outflow items to inflow items: exact txid, then address,
// then a unique amount match inside the time window.
func (b *builder) match(outs, ins []string, crossAccountOnly bool) {
	free := func(keys []string) []Item {
		var r []Item
		for _, k := range keys {
			if !b.linked[k] {
				r = append(r, b.res.Items[k])
			}
		}
		return r
	}
	ok := func(o, i Item) bool {
		if crossAccountOnly && o.Account == i.Account {
			return false
		}
		return !i.Time.Before(o.Time.Add(-6*time.Hour)) && !i.Time.After(o.Time.Add(b.opt.Window))
	}
	amountFits := func(o, i Item) bool {
		return i.Sats == o.Sats || (o.FeeSats > 0 && i.Sats == o.Sats-o.FeeSats)
	}
	for _, o := range free(outs) {
		for _, i := range free(ins) {
			if o.Txid != "" && o.Txid == i.Txid && (i.Sats <= o.Sats+o.FeeSats) {
				b.link(o.Key, i.Key, "txid")
				break
			}
		}
	}
	for _, o := range free(outs) {
		if o.Address == "" {
			continue
		}
		for _, i := range free(ins) {
			if o.Address == i.Address && ok(o, i) {
				b.link(o.Key, i.Key, "address")
				break
			}
		}
	}
	for _, o := range free(outs) {
		var cands []Item
		for _, i := range free(ins) {
			if ok(o, i) && amountFits(o, i) {
				cands = append(cands, i)
			}
		}
		if len(cands) != 1 {
			continue
		}
		// Unique in the other direction too, or a guess could steal a match.
		back := 0
		for _, o2 := range free(outs) {
			if ok(o2, cands[0]) && amountFits(o2, cands[0]) {
				back++
			}
		}
		if back == 1 {
			b.link(o.Key, cands[0].Key, "amount")
		}
	}
}

func (b *builder) link(from, to, method string) {
	o, ok1 := b.res.Items[from]
	i, ok2 := b.res.Items[to]
	if !ok1 || !ok2 || b.linked[from] || b.linked[to] {
		return
	}
	if o.Dir == In && i.Dir == Out { // manual links may come either way round
		o, i = i, o
	}
	b.linked[o.Key], b.linked[i.Key] = true, true
	b.res.Links = append(b.res.Links, Link{o.Key, i.Key, method})
	debit := o.Sats + o.FeeSats
	if o.FeeSats > 0 && i.Sats == o.Sats-o.FeeSats {
		debit = o.Sats // the platform's amount already included the fee
	}
	fee := debit - i.Sats
	if fee < 0 {
		b.warn("%s -> %s: more arrived (%d) than left (%d); check both records", o.Key, i.Key, i.Sats, debit)
		fee = 0
	}
	sats := min(i.Sats, debit)
	b.op(lots.Op{ID: o.Key + ">" + i.Key, Kind: lots.Transfer, Time: o.Time, Account: o.Account, To: i.Account, Sats: sats, FeeSats: fee})
}

func (b *builder) applyLabel(it Item, l Label) {
	total := it.Sats + it.FeeSats
	op := lots.Op{ID: it.Key, Time: it.Time, Account: it.Account, Sats: total}
	switch it.Dir {
	case Out:
		switch l.Category {
		case Spend:
			op.Kind, op.Cents = lots.Dispose, b.fmv(it.Key, it.Time, it.Sats, l.FiatCents)
		case Sold:
			op.Kind, op.Cents = lots.Dispose, l.FiatCents
		case Gift, Donation, Lost:
			op.Kind = lots.Remove
		case Untracked:
			op.Kind, op.To, op.Sats, op.FeeSats = lots.Transfer, "untracked", it.Sats, it.FeeSats
		default:
			b.warn("%s: label %q does not apply to an outflow", it.Key, l.Category)
			b.res.Unmatched = append(b.res.Unmatched, it)
			return
		}
	case In:
		op.Sats = it.Sats
		switch l.Category {
		case IncomeIn:
			b.income(it.Key, it.Account, it.Time, it.Sats, l.FiatCents, "income")
			return
		case GiftIn:
			// Carryover basis and holding period from the donor. (US rule for
			// gifts at a loss uses the lower FMV; flagged in the UI.)
			op.Kind, op.Cents = lots.Acquire, l.FiatCents
			if !l.AcquiredAt.IsZero() {
				op.Time = l.AcquiredAt
			}
		case PurchaseIn, RefundIn:
			op.Kind, op.Cents = lots.Acquire, l.FiatCents
		case Untracked:
			// Coins from a wallet we do not track: basis unknown unless given.
			op.Kind, op.Cents = lots.Acquire, l.FiatCents
			if l.FiatCents == 0 {
				b.warn("%s: from an untracked wallet with no basis entered; zero basis assumed", it.Key)
			}
		default:
			b.warn("%s: label %q does not apply to an inflow", it.Key, l.Category)
			b.res.Unmatched = append(b.res.Unmatched, it)
			return
		}
	}
	b.op(op)
}

func mulDiv(a, b, c int64) int64 { return lots.MulDiv(a, b, c) }
