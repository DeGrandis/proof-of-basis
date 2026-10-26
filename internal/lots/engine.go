// Package lots is the cost-basis engine. It replays acquisitions,
// transfers and disposals in time order and produces realised gains.
// All amounts are integers: satoshis and fiat cents.
package lots

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/tax"
)

type Kind string

const (
	Acquire  Kind = "acquire"  // buy, income, gift received: creates a lot
	Dispose  Kind = "dispose"  // sell, spend: realises gain/loss
	Transfer Kind = "transfer" // between the user's own accounts
	Remove   Kind = "remove"   // gift sent, donation, loss: lots leave with no gain
)

// Pick selects coins for a sale: a lot by id, or by the acquisition it
// came from (OriginID), which stays the same however the lot is split.
type Pick struct {
	LotID    string
	OriginID string `json:",omitempty"`
	Sats     int64
}

// Op is one normalised event fed to the engine.
type Op struct {
	ID      string
	Kind    Kind
	Time    time.Time
	Account string
	To      string // Transfer destination
	Sats    int64
	// Cents is the basis for Acquire (including fees paid) or gross
	// proceeds for Dispose.
	Cents int64
	// FeeCents reduces proceeds on Dispose. On Transfer with FeeAsDisposal
	// it is the fair market value of the network fee.
	FeeCents int64
	// FeeSats is the network fee on a Transfer, taken from the source.
	FeeSats int64
	// Picks selects lots for Specific identification.
	Picks []Pick
}

type FeeMode string

const (
	// FeeCarry moves the fee's basis onto the transferred coins (no gain).
	FeeCarry FeeMode = "carry"
	// FeeAsDisposal treats the network fee as a small disposal at FMV.
	FeeAsDisposal FeeMode = "dispose"
)

type Config struct {
	Rules tax.Rules
	// Method picks the lot-selection method for a tax year.
	Method  func(year int) tax.Method
	FeeMode FeeMode
	// Protected reports acquisitions (by origin op id) whose coins should be
	// used only when nothing else in the account is left. Specific picks
	// still take them when asked.
	Protected func(originID string) bool
	// AllocationPrefs assigns pooled purchases (by origin op id) to an
	// account at the switch to per-account tracking (Rev. Proc. 2024-28),
	// before the default fill.
	AllocationPrefs map[string]string
}

type Lot struct {
	ID       string
	OriginID string // the Acquire op this lot came from
	Account  string // empty while basis is tracked universally
	Acquired time.Time
	Sats     int64
	Basis    int64
	// Path is the transfers (op ids) these coins went through since the
	// acquisition, oldest first: their provenance.
	Path []string `json:",omitempty"`
}

type Disposal struct {
	OpID     string
	LotID    string
	OriginID string
	Account  string
	Acquired time.Time
	Disposed time.Time
	Sats     int64
	Proceeds int64
	Basis    int64
	LongTerm bool
	// Missing is set when there were not enough lots; basis is zero.
	Missing bool
	// Path is the transfers the coins went through before this sale.
	Path []string `json:",omitempty"`
}

func (d Disposal) Gain() int64 { return d.Proceeds - d.Basis }

type Warning struct {
	OpID    string
	Message string
}

// Allocation records how the universal pool was assigned to accounts when
// per-account tracking began.
type Allocation struct {
	LotID, OriginID, Account string
	Acquired                 time.Time
	Sats, Basis              int64
}

// Removal is basis that left without a sale (gift, donation, loss).
type Removal struct {
	OpID, LotID, OriginID, Account string
	Acquired, Removed              time.Time
	Sats, Basis                    int64
}

type Result struct {
	Open        []Lot
	Disposals   []Disposal
	Removals    []Removal
	Warnings    []Warning
	Balances    map[string]int64
	Allocations []Allocation
	// Dips: the first operation that took each account below zero, which
	// points at a record missing or misread just before it.
	Dips []Dip
}

// Dip is an account's balance going negative.
type Dip struct {
	Account string
	OpID    string
	Time    time.Time
	Before  int64 // the balance just before the operation
	Balance int64 // right after it
}

type engine struct {
	cfg      Config
	lots     []*Lot
	res      *Result
	perAcct  bool
	splitSeq map[string]int
}

func Run(ops []Op, cfg Config) (*Result, error) {
	if cfg.Rules == nil {
		cfg.Rules = tax.US{}
	}
	if cfg.Method == nil {
		cfg.Method = func(int) tax.Method { return tax.FIFO }
	}
	if cfg.FeeMode == "" {
		cfg.FeeMode = FeeCarry
	}
	sorted := append([]Op(nil), ops...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Time.Equal(sorted[j].Time) {
			return sorted[i].Time.Before(sorted[j].Time)
		}
		// Same timestamp: acquisitions first so a buy-then-withdraw pair works.
		return rank(sorted[i].Kind) < rank(sorted[j].Kind)
	})
	e := &engine{cfg: cfg, res: &Result{Balances: map[string]int64{}}, splitSeq: map[string]int{}}
	cut := cfg.Rules.PerAccountFrom()
	e.perAcct = cut.IsZero()
	dipped := map[string]bool{}
	for _, op := range sorted {
		if op.Sats < 0 || op.FeeSats < 0 {
			return nil, fmt.Errorf("op %s: negative amount", op.ID)
		}
		if !e.perAcct && !op.Time.Before(cut) {
			e.switchToPerAccount()
		}
		before := e.res.Balances[op.Account]
		switch op.Kind {
		case Acquire:
			e.acquire(op)
		case Dispose:
			e.dispose(op)
		case Transfer:
			e.transfer(op)
		case Remove:
			e.remove(op)
		default:
			return nil, fmt.Errorf("op %s: unknown kind %q", op.ID, op.Kind)
		}
		if b := e.res.Balances[op.Account]; b < 0 && !dipped[op.Account] {
			dipped[op.Account] = true
			e.res.Dips = append(e.res.Dips, Dip{op.Account, op.ID, op.Time, before, b})
		}
	}
	if !e.perAcct && !cut.IsZero() && time.Now().After(cut) {
		e.switchToPerAccount()
	}
	for _, l := range e.lots {
		if l.Sats > 0 {
			e.res.Open = append(e.res.Open, *l)
		}
	}
	return e.res, nil
}

func rank(k Kind) int {
	switch k {
	case Acquire:
		return 0
	case Transfer:
		return 1
	}
	return 2 // disposals and removals last
}

func (e *engine) warn(op, format string, a ...any) {
	e.res.Warnings = append(e.res.Warnings, Warning{op, fmt.Sprintf(format, a...)})
}

func (e *engine) acctKey(a string) string {
	if e.perAcct {
		return a
	}
	return ""
}

func (e *engine) acquire(op Op) {
	e.lots = append(e.lots, &Lot{ID: op.ID, OriginID: op.ID, Account: e.acctKey(op.Account),
		Acquired: op.Time, Sats: op.Sats, Basis: op.Cents})
	e.res.Balances[op.Account] += op.Sats
}

// piece is part of a lot consumed by an operation.
type piece struct {
	lot   *Lot // nil when lots ran out
	sats  int64
	basis int64
}

// take removes sats from the account's lots using the year's method.
func (e *engine) take(op Op, account string, sats int64, picks []Pick) []piece {
	key := e.acctKey(account)
	var out []piece
	remaining := sats
	consume := func(l *Lot, n int64) {
		if n > l.Sats {
			n = l.Sats
		}
		if n <= 0 {
			return
		}
		b := mulDiv(l.Basis, n, l.Sats)
		l.Sats -= n
		l.Basis -= b
		remaining -= n
		out = append(out, piece{l, n, b})
	}
	method := e.cfg.Method(op.Time.Year())
	// Coins the user identified come first, whatever the year's method.
	for _, p := range picks {
		want := p.Sats
		for _, l := range e.lots {
			if remaining == 0 || want == 0 {
				break
			}
			if l.Account != key || l.Sats == 0 || l.Acquired.After(op.Time) {
				continue
			}
			if (p.LotID != "" && l.ID == p.LotID) || (p.OriginID != "" && l.OriginID == p.OriginID) {
				before := remaining
				consume(l, min(want, remaining))
				want -= before - remaining
			}
		}
	}
	if remaining > 0 && len(picks) > 0 {
		e.warn(op.ID, "the coins identified covered only part of the amount; the rest followed the year's method")
	}
	if method == tax.Specific {
		method = tax.FIFO // nothing identified: first in, first out
	}
	cand := make([]*Lot, 0, len(e.lots))
	for _, l := range e.lots {
		if l.Account == key && l.Sats > 0 && !l.Acquired.After(op.Time) {
			cand = append(cand, l)
		}
	}
	prot := func(l *Lot) bool { return e.cfg.Protected != nil && e.cfg.Protected(l.OriginID) }
	sort.SliceStable(cand, func(i, j int) bool {
		if pi, pj := prot(cand[i]), prot(cand[j]); pi != pj {
			return pj // unprotected first
		}
		if method == tax.LIFO {
			return cand[i].Acquired.After(cand[j].Acquired)
		}
		return cand[i].Acquired.Before(cand[j].Acquired)
	})
	for _, l := range cand {
		if remaining == 0 {
			break
		}
		if prot(l) && l.Sats > 0 {
			e.warn(op.ID, "used protected coins from %s because nothing else was left in %s", l.OriginID, account)
		}
		consume(l, remaining)
	}
	if remaining > 0 {
		where := account
		if key == "" {
			where = "all accounts"
		}
		e.warn(op.ID, "not enough acquired BTC in %s: %d sats have no known basis (missing import or unlabeled deposit?)", where, remaining)
		out = append(out, piece{nil, remaining, 0})
	}
	return out
}

func (e *engine) dispose(op Op) {
	pieces := e.take(op, op.Account, op.Sats, op.Picks)
	e.realise(op, op.Account, pieces, op.Cents-op.FeeCents)
	e.res.Balances[op.Account] -= op.Sats
}

func (e *engine) remove(op Op) {
	for _, p := range e.take(op, op.Account, op.Sats, op.Picks) {
		r := Removal{OpID: op.ID, Account: op.Account, Removed: op.Time, Sats: p.sats, Basis: p.basis}
		if p.lot != nil {
			r.LotID, r.OriginID, r.Acquired = p.lot.ID, p.lot.OriginID, p.lot.Acquired
		}
		e.res.Removals = append(e.res.Removals, r)
	}
	e.res.Balances[op.Account] -= op.Sats
}

func (e *engine) realise(op Op, account string, pieces []piece, proceeds int64) {
	var total, used int64
	for _, p := range pieces {
		total += p.sats
	}
	for i, p := range pieces {
		pr := mulDiv(proceeds, p.sats, total)
		if i == len(pieces)-1 {
			pr = proceeds - used
		}
		used += pr
		d := Disposal{OpID: op.ID, Account: account, Disposed: op.Time, Sats: p.sats, Proceeds: pr, Basis: p.basis}
		if p.lot == nil {
			d.Missing, d.Acquired = true, op.Time
		} else {
			d.LotID, d.OriginID, d.Acquired, d.Path = p.lot.ID, p.lot.OriginID, p.lot.Acquired, p.lot.Path
			d.LongTerm = e.cfg.Rules.LongTerm(p.lot.Acquired, op.Time)
		}
		e.res.Disposals = append(e.res.Disposals, d)
	}
}

func (e *engine) transfer(op Op) {
	e.res.Balances[op.Account] -= op.Sats + op.FeeSats
	e.res.Balances[op.To] += op.Sats
	if !e.perAcct && op.FeeSats == 0 {
		return // universal tracking: location does not matter
	}
	// Under universal tracking the pool has no account; the coins moved are
	// still picked by the year's method so the fee's basis follows them.
	pieces := e.take(op, op.Account, op.Sats+op.FeeSats, op.Picks)
	feeLeft := op.FeeSats
	var feePieces []piece
	for _, p := range pieces {
		moveSats, moveBasis := p.sats, p.basis
		if feeLeft > 0 && e.cfg.FeeMode == FeeAsDisposal {
			// Split the fee off as its own piece; it is realised below.
			f := min(feeLeft, p.sats)
			fb := mulDiv(p.basis, f, p.sats)
			feePieces = append(feePieces, piece{p.lot, f, fb})
			moveSats, moveBasis = p.sats-f, p.basis-fb
			feeLeft -= f
		} else if feeLeft > 0 {
			// Carry: fee sats vanish, their basis stays with the moved coins.
			f := min(feeLeft, p.sats)
			moveSats -= f
			feeLeft -= f
		}
		if moveSats <= 0 {
			continue
		}
		nl := &Lot{OriginID: op.ID, Account: e.acctKey(op.To), Acquired: op.Time, Sats: moveSats, Basis: moveBasis}
		if p.lot != nil {
			nl.OriginID, nl.Acquired = p.lot.OriginID, p.lot.Acquired
			nl.Path = append(append([]string(nil), p.lot.Path...), op.ID)
			e.splitSeq[p.lot.OriginID]++
			nl.ID = fmt.Sprintf("%s/%d", p.lot.OriginID, e.splitSeq[p.lot.OriginID])
		} else {
			nl.ID = op.ID + "/unknown"
		}
		e.lots = append(e.lots, nl)
	}
	if len(feePieces) > 0 {
		e.realise(op, op.Account, feePieces, op.FeeCents)
	}
}

// PoolSplit starts the path of coins that came out of the universal pool:
// where they were before is not tracked, only the account they were given
// to at the switch (PoolSplit + account).
const PoolSplit = "pool-split:"

// switchToPerAccount assigns the universal pool to accounts at the
// Rev. Proc. 2024-28 transition, up to each account's actual balance:
// first the purchases the user assigned (AllocationPrefs), then the
// default, oldest lots first, filling accounts in name order.
func (e *engine) switchToPerAccount() {
	e.perAcct = true
	var pool []*Lot
	for _, l := range e.lots {
		if l.Account == "" && l.Sats > 0 {
			pool = append(pool, l)
		}
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].Acquired.Before(pool[j].Acquired) })
	var accts []string
	need := map[string]int64{}
	for a, bal := range e.res.Balances {
		if bal > 0 {
			accts = append(accts, a)
			need[a] = bal
		}
	}
	sort.Strings(accts)
	var kept []*Lot
	for _, l := range e.lots {
		if l.Account != "" || l.Sats == 0 {
			kept = append(kept, l)
		}
	}
	assign := func(l *Lot, a string) {
		n := min(need[a], l.Sats)
		if n <= 0 {
			return
		}
		b := mulDiv(l.Basis, n, l.Sats)
		e.splitSeq[l.OriginID]++
		nl := &Lot{ID: fmt.Sprintf("%s/%d", l.OriginID, e.splitSeq[l.OriginID]), OriginID: l.OriginID,
			Account: a, Acquired: l.Acquired, Sats: n, Basis: b, Path: []string{PoolSplit + a}}
		kept = append(kept, nl)
		e.res.Allocations = append(e.res.Allocations, Allocation{nl.ID, nl.OriginID, a, nl.Acquired, n, b})
		l.Sats -= n
		l.Basis -= b
		need[a] -= n
	}
	for _, l := range pool {
		if a, ok := e.cfg.AllocationPrefs[l.OriginID]; ok && need[a] > 0 {
			assign(l, a)
		}
	}
	for _, a := range accts {
		for _, l := range pool {
			if need[a] == 0 {
				break
			}
			if l.Sats > 0 {
				assign(l, a)
			}
		}
	}
	for _, l := range pool {
		if l.Sats > 0 {
			e.warn(l.ID, "%d sats of basis could not be allocated to any account at the per-wallet transition", l.Sats)
		}
	}
	e.lots = kept
}

// MulDiv returns a*b/c rounded to nearest, without overflow.
func MulDiv(a, b, c int64) int64 { return mulDiv(a, b, c) }

func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	if b == c {
		return a
	}
	x := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	x.Add(x, big.NewInt(c/2))
	return x.Quo(x, big.NewInt(c)).Int64()
}
