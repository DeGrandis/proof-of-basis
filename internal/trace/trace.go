// Package trace links exchange records to on-chain wallet activity,
// builds the labeling queue for what it cannot prove, and turns the result
// into operations for the lot engine.
package trace

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"btctax/internal/importers"
	"btctax/internal/lightning"
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
	// FeeApprox: FeeSats is the export's USD fee in sats, which the chain
	// receipt shows to be on top of the amount or inside it.
	FeeApprox bool
	Address   string
	Txid      string
	Kind      string // importer kind for exchange items
	Note      string
	Network   string // "onchain", "lightning", or "" when the record doesn't say
	Value     int64  // market value in USD cents at the day's price, 0 if unknown
	// Default is how an unmatched item counts when the user hasn't labeled
	// it (Lightning payments are spends, receipts income); empty means it
	// waits in Review.
	Default Category
}

// network guesses how an exchange record moved: Lightning invoices and
// addresses, or an on-chain txid or address.
func network(it Item) string {
	a := strings.ToLower(strings.TrimSpace(it.Address))
	n := strings.ToLower(it.Note + " " + it.Kind)
	switch {
	case it.Source == "chain":
		return "onchain"
	case strings.HasPrefix(a, "lnbc"), strings.HasPrefix(a, "lntb"), strings.HasPrefix(a, "lnurl"), strings.HasPrefix(a, "lightning:"),
		strings.Contains(a, "@"), strings.Contains(n, "lightning"):
		return "lightning"
	case it.Txid != "", strings.HasPrefix(a, "bc1"), strings.HasPrefix(a, "tb1"), strings.HasPrefix(a, "bcrt1"),
		strings.HasPrefix(a, "1"), strings.HasPrefix(a, "3"), strings.Contains(n, "on-chain"), strings.Contains(n, "onchain"):
		return "onchain"
	}
	return ""
}

type Link struct {
	From, To string // item keys; From is the outflow
	Method   string // txid | address | amount | internal | manual
	FeeSats  int64  // network fee the transfer paid: what left less what arrived
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
	// SelfTransfer moves coins to or from a wallet of mine that is not added
	// here. Lots keep their basis and acquisition dates: out, they go to the
	// "untracked" account; in, they come back from it.
	SelfTransfer Category = "self-transfer"
)

// UntrackedAccount holds lots sent to wallets of mine that are not added.
const UntrackedAccount = "untracked"

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

// TxInfo is the shape of an on-chain transaction, for display.
type TxInfo struct {
	Inputs, Outputs int
	OutSats         int64
}

type Result struct {
	Txs       map[string]TxInfo
	Defaults  map[string]Category // items counted by their default category
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
	// Lightning is a Core Lightning node's bookkeeper history, tracked as
	// one account named LightningAccount.
	Lightning        []lightning.Event
	LightningAccount string
}

func Build(ex []ExEvent, wallets []WalletTxs, labels map[string]Label, opt Options) *Result {
	if opt.Window == 0 {
		opt.Window = 72 * time.Hour
	}
	if opt.Price == nil {
		opt.Price = func(time.Time) (int64, bool) { return 0, false }
	}
	b := &builder{res: &Result{Items: map[string]Item{}, Txs: map[string]TxInfo{}, Defaults: map[string]Category{}}, opt: opt, linked: map[string]bool{}}
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
			b.income(key, e.Account, e.Time, e.Sats, IncomeValue(e.Event, labels[key]), string(e.Kind))
		case importers.Withdrawal, importers.Deposit:
			it := Item{Key: key, Source: "exchange", Account: e.Account, Time: e.Time, Sats: e.Sats,
				FeeSats: e.FeeSats, FeeApprox: e.FeeApprox, Address: e.Address, Txid: e.Txid, Kind: string(e.Kind), Note: e.Note, Dir: In}
			if e.Kind == importers.Withdrawal {
				it.Dir = Out
				withdrawals = append(withdrawals, key)
			} else {
				deposits = append(deposits, key)
			}
			b.res.Items[key] = it
		}
	}
	lnW, lnD, lnIn, lnOut := b.lightningItems(opt.Lightning, opt.LightningAccount)
	withdrawals = append(withdrawals, lnW...)
	deposits = append(deposits, lnD...)
	for k, it := range b.res.Items {
		if it.Network == "" {
			it.Network = network(it)
		}
		if p, ok := opt.Price(it.Time); ok {
			it.Value = mulDiv(p, it.Sats, 100_000_000)
		}
		b.res.Items[k] = it
	}
	// Manual links first: the user's word beats any heuristic.
	for _, l := range labels {
		if l.Category == SelfLink && l.Counterpart != "" {
			b.link(l.Key, l.Counterpart, "manual")
		}
	}
	ins, outs := append(b.chainKeys(In), lnIn...), append(b.chainKeys(Out), lnOut...)
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
		if it.Default != "" {
			b.res.Defaults[k] = it.Default
			b.applyLabel(it, Label{Key: k, Category: it.Default})
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

func (b *builder) op(o lots.Op) {
	// A network fee on a transfer is valued at the day's price, in case the
	// user treats fees as small disposals instead of adding them to basis.
	if o.Kind == lots.Transfer && o.FeeSats > 0 && o.FeeCents == 0 {
		if p, ok := b.opt.Price(o.Time); ok {
			o.FeeCents = mulDiv(p, o.FeeSats, 100_000_000)
		}
	}
	b.res.Ops = append(b.res.Ops, o)
}

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

// IncomeValue is what an imported income record is worth: the value the
// user set on it (an income label with an amount) over the export's.
func IncomeValue(e importers.Event, l Label) int64 {
	if l.Category == IncomeIn && l.FiatCents > 0 {
		return l.FiatCents
	}
	return e.FiatCents
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
		ourIns int // inputs claimed by any tracked wallet
	}
	txs := map[string]*agg{}
	var order []string
	// If two tracked wallets share addresses (the same key added twice, or
	// overlapping descriptors), the first wallet owns them and the other
	// sees nothing, so nothing is counted twice or turned into a transfer.
	spentBy := map[string]string{} // "txid:vout" of a spent prevout -> wallet
	overlap := map[[2]string]bool{}
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
			for _, in := range t.Inputs {
				if !in.Ours {
					continue
				}
				k := fmt.Sprintf("%s:%d", in.PrevTxid, in.PrevVout)
				if prev, ok := spentBy[k]; ok && prev != w.Account {
					overlap[[2]string{prev, w.Account}] = true
					continue
				}
				spentBy[k] = w.Account
				a.sent[w.Account] += in.Sats
				a.ourIns++
			}
			if t.SentSats > 0 && !hasOurInput(t) { // summaries without input detail
				a.sent[w.Account] += t.SentSats
			}
			for _, o := range t.Outputs {
				if !o.Ours {
					continue
				}
				if prev, ok := a.owner[o.Vout]; ok && prev != w.Account {
					overlap[[2]string{prev, w.Account}] = true
					continue
				}
				a.owner[o.Vout] = w.Account
			}
			if t.FeeSats > 0 {
				a.fee, a.feeSet = t.FeeSats, true
			}
		}
	}
	pairs := make([][2]string, 0, len(overlap))
	for p := range overlap {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0]+pairs[i][1] < pairs[j][0]+pairs[j][1] })
	for _, p := range pairs {
		b.warn("wallets %q and %q share addresses (the same wallet added twice?); %q is ignored where they overlap. Remove one of them.", p[0], p[1], p[1])
	}
	for _, id := range order {
		a := txs[id]
		t := a.tx
		key := func(v uint32) string { return fmt.Sprintf("tx:%s:%d", id, v) }
		var outSats int64
		for _, o := range t.Outputs {
			outSats += o.Sats
		}
		b.res.Txs[id] = TxInfo{Inputs: len(t.Inputs), Outputs: len(t.Outputs), OutSats: outSats}
		// Inputs from several tracked wallets together cover the whole fee
		// even though no single wallet's view does.
		if !a.feeSet && a.ourIns > 0 && a.ourIns == len(t.Inputs) {
			var in int64
			for _, v := range a.sent {
				in += v
			}
			if in >= outSats {
				a.fee, a.feeSet = in-outSats, true
			}
		}
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
		// Many to one: coins from the other tracked wallets that funded this
		// transaction move to the main sender first, so each wallet's lots
		// and balance stay right; then everything leaves from the sender.
		others := make([]string, 0, len(a.sent))
		for acct := range a.sent {
			if acct != sender {
				others = append(others, acct)
			}
		}
		sort.Strings(others)
		for _, acct := range others {
			b.op(lots.Op{ID: key(0) + ":from:" + acct, Kind: lots.Transfer, Time: t.Time, Account: acct, To: sender, Sats: a.sent[acct]})
		}
		if !a.feeSet {
			b.warn("tx %s has inputs from outside the tracked wallets (coinjoin or payjoin?); review it", id)
		}
		fee := a.fee
		// With inputs from outside the tracked wallets, part of what left was
		// never ours: the wallets lose at most what they put in, less their
		// change. Outputs take from that in order; the rest was someone
		// else's (or from an address no scan found).
		budget := int64(-1)
		if !a.feeSet {
			budget = 0
			for _, v := range a.sent {
				budget += v
			}
			for _, o := range t.Outputs {
				if acct, ours := a.owner[o.Vout]; ours && acct == sender {
					budget -= o.Sats
				}
			}
			budget = max(budget, 0)
		}
		take := func(sats int64) int64 {
			if budget < 0 {
				return sats
			}
			n := min(sats, budget)
			budget -= n
			return n
		}
		var internal []lots.Op
		for _, o := range t.Outputs {
			acct, ours := a.owner[o.Vout]
			switch {
			case ours && acct == sender:
				// change
			case ours:
				if n := take(o.Sats); n > 0 {
					internal = append(internal, lots.Op{ID: key(o.Vout), Kind: lots.Transfer, Time: t.Time,
						Account: sender, To: acct, Sats: n})
				}
			default:
				n := take(o.Sats)
				if n == 0 {
					continue
				}
				it := Item{Key: key(o.Vout), Source: "chain", Account: sender, Dir: Out, Time: t.Time,
					Sats: n, Address: o.Address, Txid: id}
				if n < o.Sats {
					it.Note = fmt.Sprintf("Only %d of this output's %d sats came from your tracked wallets; the rest came from inputs no wallet here owns (an address the scan never found, or a coinjoin)", n, o.Sats)
				}
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

// lightningItems turns a node's bookkeeper events into items and ops. Its
// on-chain deposits and withdrawals are matched like an exchange's (by txid
// with your wallets); payments and receipts are matched by amount with
// exchange Lightning records, and otherwise count as spends and income.
// Routing fees earned are income; fees the node pays leave as fees.
func (b *builder) lightningItems(evs []lightning.Event, acct string) (withdrawals, deposits, ins, outs []string) {
	if acct == "" {
		acct = "Lightning node"
	}
	fees := map[string]int64{}
	for _, e := range evs {
		if e.Tag == "invoice_fee" && e.PaymentID != "" {
			fees[e.PaymentID] += e.DebitSats
		}
	}
	used := map[string]bool{}
	for _, e := range evs {
		key := "ln:" + e.UID
		t := time.Unix(e.Time, 0).UTC()
		it := Item{Key: key, Source: "lightning", Account: acct, Time: t, Txid: e.Txid, Note: e.Description, Kind: e.Tag}
		switch {
		case e.Tag == "deposit" && e.CreditSats > 0:
			it.Dir, it.Sats, it.Network, it.Kind = In, e.CreditSats, "onchain", "node deposit"
			b.res.Items[key] = it
			deposits = append(deposits, key)
		case e.Tag == "withdrawal" && e.DebitSats > 0:
			it.Dir, it.Sats, it.Network, it.Kind = Out, e.DebitSats, "onchain", "node withdrawal"
			b.res.Items[key] = it
			withdrawals = append(withdrawals, key)
		case e.Tag == "invoice" && e.CreditSats > 0:
			it.Dir, it.Sats, it.Network, it.Kind, it.Default = In, e.CreditSats, "lightning", "lightning received", IncomeIn
			b.res.Items[key] = it
			ins = append(ins, key)
		case e.Tag == "invoice" && e.DebitSats > 0:
			it.Dir, it.Sats, it.Network, it.Kind, it.Default = Out, e.DebitSats, "lightning", "lightning payment", Spend
			if e.PaymentID != "" && !used[e.PaymentID] {
				it.FeeSats, used[e.PaymentID] = fees[e.PaymentID], true
			}
			b.res.Items[key] = it
			outs = append(outs, key)
		case e.Tag == "invoice_fee":
			// carried on its payment above; a fee with no payment leaves alone
			if e.PaymentID == "" || !hasPayment(evs, e.PaymentID) {
				b.op(lots.Op{ID: key, Kind: lots.Transfer, Time: t, Account: acct, To: acct, FeeSats: e.DebitSats})
			}
		case (e.Tag == "routed" || e.Tag == "lease_fee") && e.CreditSats > 0:
			b.income(key, acct, t, e.CreditSats, 0, "routing")
		case e.DebitSats > 0 && strings.HasSuffix(e.Tag, "fee"):
			b.op(lots.Op{ID: key, Kind: lots.Transfer, Time: t, Account: acct, To: acct, FeeSats: e.DebitSats})
		case e.CreditSats > 0 || e.DebitSats > 0:
			b.warn("%s: Lightning %q event of %d/%d sats isn't handled yet; check it", key, e.Tag, e.CreditSats, e.DebitSats)
		}
	}
	return
}

func hasPayment(evs []lightning.Event, id string) bool {
	for _, e := range evs {
		if e.Tag == "invoice" && e.PaymentID == id && e.DebitSats > 0 {
			return true
		}
	}
	return false
}

func hasOurInput(t scan.Tx) bool {
	for _, in := range t.Inputs {
		if in.Ours {
			return true
		}
	}
	return false
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
	within := func(win time.Duration) func(o, i Item) bool {
		return func(o, i Item) bool {
			if o.Account == i.Account {
				return false // a record can't be a transfer to its own account
			}
			return !i.Time.Before(o.Time.Add(-6*time.Hour)) && !i.Time.After(o.Time.Add(win))
		}
	}
	ok := within(b.opt.Window)
	// An exchange's send can sit unconfirmed for days when fees spike (the
	// 2017 backlog), and a chain receipt carries its block time. Those get
	// a second look over the longest a node keeps a transaction waiting.
	slow := within(max(b.opt.Window, slowConfirm))
	exToChain := func(fits func(o, i Item) bool) func(o, i Item) bool {
		return func(o, i Item) bool { return o.Source == "exchange" && i.Source == "chain" && slow(o, i) && fits(o, i) }
	}
	slowSend := exToChain(func(Item, Item) bool { return true })
	// unique links each outflow to the one inflow that fits it, when that
	// inflow fits no other outflow either, or a guess could steal a match.
	unique := func(fits func(o, i Item) bool, method string) {
		for _, o := range free(outs) {
			var cands []Item
			for _, i := range free(ins) {
				if fits(o, i) {
					cands = append(cands, i)
				}
			}
			if len(cands) != 1 {
				continue
			}
			back := 0
			for _, o2 := range free(outs) {
				if fits(o2, cands[0]) {
					back++
				}
			}
			if back == 1 {
				b.link(o.Key, cands[0].Key, method)
			}
		}
	}
	amountFits := func(o, i Item) bool {
		return i.Sats == o.Sats || (o.FeeSats > 0 && i.Sats == o.Sats-o.FeeSats)
	}
	// Some exports (old Coinbase sends) give the amount debited with the
	// network fee folded in and no fee of their own, so the receipt is
	// smaller by an unknown fee, which in a fee spike can exceed what
	// arrived. A fee given only in USD may sit inside the amount the same
	// way, off by rounding. Accept a receipt short by at most maxHiddenFee;
	// the difference becomes the fee.
	hiddenFee := func(o, i Item) bool {
		return o.Source == "exchange" && (o.FeeSats == 0 || o.FeeApprox) && i.Sats < o.Sats && o.Sats-i.Sats <= maxHiddenFee
	}
	for _, o := range free(outs) {
		for _, i := range free(ins) {
			if o.Txid != "" && o.Txid == i.Txid && o.Account != i.Account && (i.Sats <= o.Sats+o.FeeSats) {
				b.link(o.Key, i.Key, "txid")
				break
			}
		}
	}
	// Same address: a reused one can have several receipts in the window,
	// and sends can confirm out of order, so the amount decides: exact
	// first, then short by a hidden fee, then any receipt no bigger than
	// what left. Never one where more arrived than left, or two sends to
	// one address could swap receipts.
	sameAddr := func(o, i Item) bool {
		return o.Address != "" && o.Address == i.Address && (ok(o, i) || slowSend(o, i))
	}
	covered := func(o, i Item) bool { return i.Sats <= o.Sats+o.FeeSats }
	for _, fits := range []func(o, i Item) bool{amountFits, hiddenFee, covered} {
		for _, o := range free(outs) {
			for _, i := range free(ins) {
				if sameAddr(o, i) && fits(o, i) {
					b.link(o.Key, i.Key, "address")
					break
				}
			}
		}
	}
	unique(func(o, i Item) bool { return ok(o, i) && amountFits(o, i) }, "amount")
	unique(func(o, i Item) bool { return ok(o, i) && hiddenFee(o, i) }, "amount less network fee")
	// Then the slow-confirming exchange sends, in the same order.
	unique(exToChain(amountFits), "amount")
	unique(exToChain(hiddenFee), "amount less network fee")
}

// maxHiddenFee bounds the fee inferred when a withdrawal's amount includes
// it: 0.01 BTC, above any per-transaction fee seen even in the 2017 spike.
const maxHiddenFee = 1_000_000

// slowConfirm is how long after an exchange's send its receipt may confirm:
// two weeks, the default time a Bitcoin node keeps an unconfirmed
// transaction before dropping it.
const slowConfirm = 14 * 24 * time.Hour

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
	debit := o.Sats + o.FeeSats
	if o.FeeSats > 0 && i.Sats == o.Sats-o.FeeSats {
		debit = o.Sats // the platform's amount already included the fee
	}
	if o.FeeApprox && i.Sats < o.Sats {
		debit = o.Sats // a fee known only in USD came out of the amount
	}
	fee := debit - i.Sats
	if fee < 0 {
		b.warn("%s -> %s: more arrived (%d) than left (%d); check both records", o.Key, i.Key, i.Sats, debit)
		fee = 0
	}
	sats := min(i.Sats, debit)
	b.res.Links = append(b.res.Links, Link{o.Key, i.Key, method, fee})
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
			// Proceeds are what the buyer paid when given, else market value.
			op.Kind, op.Cents = lots.Dispose, b.fmv(it.Key, it.Time, it.Sats, l.FiatCents)
		case Gift, Donation, Lost:
			op.Kind = lots.Remove
		case Untracked, SelfTransfer:
			op.Kind, op.To, op.Sats, op.FeeSats = lots.Transfer, UntrackedAccount, it.Sats, it.FeeSats
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
		case SelfTransfer:
			// Back from my untracked wallet: the lots that went there return
			// with their basis and dates. Anything never sent there has no
			// known basis, which the engine reports.
			op.Kind, op.Account, op.To = lots.Transfer, UntrackedAccount, it.Account
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
