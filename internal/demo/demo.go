// Package demo fills an empty database with a large, invented history of
// someone who tries exchanges for months at a time (moving their weekly
// buys, dip buys and the odd sale from one to the next and sometimes back)
// and moves their savings from one hardware wallet to the next, spending
// from a phone wallet along the way. It is for trying the app and testing
// the flow view at a realistic size. Nothing in it is real.
package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/importers"
	"github.com/DeGrandis/proof-of-basis/internal/prices"
	"github.com/DeGrandis/proof-of-basis/internal/scan"
	"github.com/DeGrandis/proof-of-basis/internal/store"
	"github.com/DeGrandis/proof-of-basis/internal/trace"
)

const (
	coinbase, river, strike, kraken = "Coinbase", "River", "Strike", "Kraken"
	ledger, trezor, coldcard, phone = "Ledger", "Trezor", "Coldcard", "Phone wallet"
)

var (
	wallets   = []string{ledger, trezor, coldcard, phone}
	exchanges = []string{coinbase, river, strike, kraken}
)

// phase is a stretch on one exchange with weekly buys of lo to hi dollars.
// Phases overlap by a few weeks while moving over.
type phase struct {
	ex       string
	from, to time.Time
	lo, hi   int64
}

func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t.Add(15 * time.Hour)
}

var phases = []phase{
	{coinbase, day("2019-01-07"), day("2020-03-30"), 25, 100},
	{river, day("2020-03-02"), day("2021-09-27"), 100, 200},
	{strike, day("2021-09-06"), day("2022-12-26"), 50, 150},
	{coinbase, day("2022-12-05"), day("2023-06-26"), 150, 300}, // back for a while, buying more
	{kraken, day("2023-06-12"), day("2024-10-28"), 75, 175},
	{river, day("2024-10-07"), day("2025-08-25"), 150, 250},
	{strike, day("2025-08-04"), day("2026-09-28"), 50, 100},
}

// The hardware wallet savings go to from each date.
var hardware = []struct {
	name string
	from time.Time
}{{ledger, day("2019-01-07")}, {trezor, day("2021-05-17")}, {coldcard, day("2023-10-09")}}

func savingsOn(t time.Time) string {
	name := hardware[0].name
	for _, h := range hardware {
		if !t.Before(h.from) {
			name = h.name
		}
	}
	return name
}

type utxo struct {
	txid  string
	vout  uint32
	sats  int64
	addr  string
	owner string
}

type out struct {
	owner string // a wallet name, or "" for an address outside the tracked wallets
	sats  int64
}

type world struct {
	rnd    *rand.Rand
	price  map[string]int64
	utxos  map[string][]utxo // wallet -> unspent outputs
	views  map[string][]scan.Tx
	events map[string][]importers.Event // exchange -> rows
	bal    map[string]int64             // exchange -> sats
	labels []trace.Label
	n      int
	height int64
}

// Seed writes the demo history into st, which must be empty.
func Seed(st *store.Store, seed int64) error {
	if accts, err := st.Accounts(); err != nil {
		return err
	} else if len(accts) > 0 {
		return errors.New("the database already has accounts; demo data only goes into an empty one")
	}
	w := &world{rnd: newRand(seed), price: prices.Bundled(), utxos: map[string][]utxo{},
		views: map[string][]scan.Tx{}, events: map[string][]importers.Event{}, bal: map[string]int64{}, height: 560_000}
	w.play()

	for _, name := range wallets {
		id, err := st.AddWallet(name, "demo:"+name, "p2wpkh", 20)
		if err != nil {
			return err
		}
		var bal int64
		for _, u := range w.utxos[name] {
			bal += u.sats
		}
		if err := st.SaveScan(id, &scan.Result{Txs: w.views[name], Balance: bal, Used: map[string]bool{}}); err != nil {
			return err
		}
	}
	for _, ex := range exchanges {
		id, err := st.ExchangeAccount(ex, "demo")
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte("demo:" + ex))
		if _, _, err := st.AddImport(id, "demo", "demo-"+ex+".csv", hex.EncodeToString(sum[:]), w.events[ex], nil); err != nil {
			return err
		}
	}
	for _, l := range w.labels {
		if err := st.SetLabel(l); err != nil {
			return err
		}
	}
	return nil
}

func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

func (w *world) play() {
	start, end := phases[0].from, phases[len(phases)-1].to
	week := 0
	for t := start; t.Before(end); t = t.AddDate(0, 0, 7) {
		week++
		savings := savingsOn(t)
		// Moving to a new hardware wallet: Trezor takes everything on the
		// Ledger in one sweep; the Coldcard gets the Trezor's coins in two
		// goes, half now and the rest six weeks later.
		switch {
		case sameWeek(t, day("2021-05-17")):
			w.sweep(ledger, trezor, t.Add(5*time.Hour))
		case sameWeek(t, day("2023-10-09")):
			w.send(trezor, coldcard, w.total(trezor)/2, t.Add(5*time.Hour))
		case sameWeek(t, day("2023-11-20")):
			w.sweep(trezor, coldcard, t.Add(5*time.Hour))
		}
		var active []string
		for i, p := range phases {
			if t.Before(p.from) || !t.Before(p.to) {
				// Leaving an exchange: take everything off, now and then
				// leaving a little dust behind.
				if !t.Before(p.to) && t.Before(p.to.AddDate(0, 0, 7)) && w.bal[p.ex] > 20_000 {
					dust := int64(0)
					if w.rnd.Intn(2) == 0 {
						dust = 5_000 + w.rnd.Int63n(30_000)
					}
					w.withdraw(p.ex, savings, w.bal[p.ex]-dust, t.Add(30*time.Hour))
				}
				continue
			}
			active = append(active, p.ex)
			// The regular buy, at this phase's size.
			w.buy(p.ex, t.Add(time.Duration(i)*time.Hour), p.lo+w.rnd.Int63n(p.hi-p.lo+1))
			// Now and then a bigger buy on a dip, the same day: before the
			// month's withdrawal, which counts it.
			if w.rnd.Intn(10) == 0 {
				w.buy(p.ex, t.Add(8*time.Hour), 200+w.rnd.Int63n(1300))
			}
			// Once in a while, sell some of what's on the exchange.
			if w.rnd.Intn(30) == 0 && w.bal[p.ex] > 500_000 {
				w.sell(p.ex, w.bal[p.ex]*int64(10+w.rnd.Intn(30))/100, t.Add(4*24*time.Hour))
			}
			// Rewards: Coinbase's Earn while on Coinbase, a referral bonus
			// at River.
			if p.ex == coinbase && week%13 == 5 {
				w.event(coinbase, importers.Event{Kind: importers.Income, Time: t.Add(time.Hour), Sats: 2_000 + w.rnd.Int63n(8_000), Note: "Earn reward"})
			}
			if p.ex == river && week%40 == 11 {
				w.event(river, importers.Event{Kind: importers.Income, Time: t.Add(time.Hour), Sats: 10_000 + w.rnd.Int63n(15_000), Note: "Referral bonus"})
			}
			// Lightning payments straight from Strike, left for the Review queue.
			if p.ex == strike && week%9 == 4 && w.bal[strike] > 60_000 {
				sats := 5_000 + w.rnd.Int63n(45_000)
				w.event(strike, importers.Event{Kind: importers.Withdrawal, Time: t.Add(20 * time.Hour), Sats: sats, FeeSats: 1 + w.rnd.Int63n(20),
					Address: fmt.Sprintf("lnbc%dn1demo%06d", sats/100, w.n), Note: "Lightning payment"})
			}
		}
		// Monthly, the exchanges in use send to savings, now and then to the
		// phone wallet instead.
		if week%4 == 0 {
			for _, ex := range active {
				if w.bal[ex] > 300_000 {
					to := savings
					if w.rnd.Intn(5) == 0 && w.total(phone) < 1_000_000 {
						to = phone
					}
					w.withdraw(ex, to, w.bal[ex]-10_000-w.rnd.Int63n(50_000), t.Add(26*time.Hour))
				}
			}
		}
		// Everyday spending from the phone, topped up from savings twice a
		// year.
		if week%3 == 1 {
			w.pay(t.Add(30*time.Hour), []string{phone}, 10_000+w.rnd.Int63n(60_000))
		}
		if week%26 == 3 && w.total(savings) > 3_000_000 {
			w.send(savings, phone, 1_000_000+w.rnd.Int63n(2_000_000), t.Add(40*time.Hour))
		}
		// A big purchase once a year, paid from the phone and savings together.
		// It costs more than the phone holds, so savings chips in.
		if week%52 == 30 && w.total(phone) > 0 && w.total(savings) > 5_000_000 {
			w.pay(t.Add(70*time.Hour), []string{phone, savings}, w.total(phone)+1_000_000+w.rnd.Int63n(2_000_000))
		}
		// Once a year the savings wallet consolidates its many small coins
		// from monthly withdrawals into one.
		if week%52 == 33 {
			w.sweep(savings, savings, t.Add(90*time.Hour))
		}
		// Yearly from 2021, some savings go back to the exchange in use and
		// are sold.
		if week%52 == 40 && t.Year() >= 2021 && len(active) > 0 {
			w.depositAndSell(savings, active[len(active)-1], t.Add(110*time.Hour))
		}
	}
}

func sameWeek(t, d time.Time) bool { return !d.Before(t) && d.Before(t.AddDate(0, 0, 7)) }

func (w *world) priceOn(t time.Time) int64 {
	for i := 0; i < 10; i++ {
		if p, ok := w.price[t.AddDate(0, 0, -i).Format("2006-01-02")]; ok {
			return p
		}
	}
	// Past the end of the bundled history: use its last price.
	last := ""
	for d := range w.price {
		if d > last {
			last = d
		}
	}
	if last != "" {
		return w.price[last]
	}
	return 30_000_00
}

func (w *world) event(ex string, e importers.Event) {
	w.n++
	e.UID = fmt.Sprintf("demo:%d", w.n)
	e.Ref = e.UID
	e.Row = w.n
	// The row as an exchange's CSV would have it, shown on the record's page.
	e.Raw = map[string]string{"ID": e.UID, "Timestamp": e.Time.UTC().Format(time.RFC3339), "Type": string(e.Kind),
		"Quantity BTC": fmt.Sprintf("%.8f", float64(e.Sats)/1e8)}
	if e.FiatCents != 0 {
		e.Raw["Total USD"] = fmt.Sprintf("%.2f", float64(e.FiatCents)/100)
	}
	if e.FeeCents != 0 {
		e.Raw["Fee USD"] = fmt.Sprintf("%.2f", float64(e.FeeCents)/100)
	}
	if e.FeeSats != 0 {
		e.Raw["Network fee BTC"] = fmt.Sprintf("%.8f", float64(e.FeeSats)/1e8)
	}
	if e.Txid != "" {
		e.Raw["Transaction hash"] = e.Txid
	}
	switch e.Kind {
	case importers.Buy, importers.Income, importers.Deposit:
		w.bal[ex] += e.Sats
	case importers.Sell, importers.Withdrawal:
		w.bal[ex] -= e.Sats + e.FeeSats
	}
	w.events[ex] = append(w.events[ex], e)
}

func (w *world) buy(ex string, t time.Time, usd int64) {
	cents := usd * 100
	fee := cents / 100
	sats := (cents - fee) * 100_000_000 / w.priceOn(t)
	w.event(ex, importers.Event{Kind: importers.Buy, Time: t, Sats: sats, FiatCents: cents, FeeCents: fee, Currency: "USD"})
}

func (w *world) addr(owner string) string {
	w.n++
	if owner == "" {
		return fmt.Sprintf("bc1qdemoexternal%06d", w.n)
	}
	return fmt.Sprintf("bc1qdemo%s%06d", map[string]string{ledger: "ledger", trezor: "trezor", coldcard: "coldcard", phone: "phone"}[owner], w.n)
}

// tx records a transaction: inputs are wallet coins (external inputs are
// added when ext > 0), outputs go to wallets or outside. Each wallet that
// is involved gets its own view of it, as a scan would produce.
func (w *world) tx(t time.Time, ins []utxo, ext int, outs []out) (string, []utxo) {
	w.n++
	w.height++
	sum := sha256.Sum256([]byte(fmt.Sprint("demo-tx", w.n)))
	txid := hex.EncodeToString(sum[:])
	var made []utxo
	for i, o := range outs {
		u := utxo{txid: txid, vout: uint32(i), sats: o.sats, addr: w.addr(o.owner), owner: o.owner}
		made = append(made, u)
	}
	involved := map[string]bool{}
	for _, u := range ins {
		involved[u.owner] = true
	}
	for _, u := range made {
		if u.owner != "" {
			involved[u.owner] = true
		}
	}
	names := make([]string, 0, len(involved))
	for n := range involved {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		v := scan.Tx{Txid: txid, Height: w.height, Time: t}
		allOurs := ext == 0
		for _, u := range ins {
			mine := u.owner == name
			in := scan.Input{PrevTxid: u.txid, PrevVout: u.vout, Ours: mine}
			if mine {
				in.Sats = u.sats
				v.SentSats += u.sats
			} else {
				allOurs = false
			}
			v.Inputs = append(v.Inputs, in)
		}
		for i := 0; i < ext; i++ {
			v.Inputs = append(v.Inputs, scan.Input{PrevTxid: fmt.Sprintf("%064d", w.n*10+i)})
		}
		var outTotal int64
		for _, u := range made {
			mine := u.owner == name
			v.Outputs = append(v.Outputs, scan.Output{Vout: u.vout, Address: u.addr, Sats: u.sats, Ours: mine})
			outTotal += u.sats
			if mine {
				v.RecvSats += u.sats
			}
		}
		if allOurs && len(ins) > 0 {
			v.FeeSats = v.SentSats - outTotal
		}
		w.views[name] = append(w.views[name], v)
	}
	for _, u := range ins {
		w.spent(u)
	}
	for _, u := range made {
		if u.owner != "" {
			w.utxos[u.owner] = append(w.utxos[u.owner], u)
		}
	}
	return txid, made
}

func (w *world) spent(u utxo) {
	list := w.utxos[u.owner]
	for i, x := range list {
		if x.txid == u.txid && x.vout == u.vout {
			w.utxos[u.owner] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

func (w *world) total(name string) int64 {
	var s int64
	for _, u := range w.utxos[name] {
		s += u.sats
	}
	return s
}

// pick takes coins from a wallet, oldest first, until amount is covered.
func (w *world) pick(name string, amount int64) ([]utxo, int64) {
	var got []utxo
	var sum int64
	for _, u := range w.utxos[name] {
		if sum >= amount {
			break
		}
		got = append(got, u)
		sum += u.sats
	}
	return got, sum
}

func (w *world) fee(inputs, outputs int) int64 {
	return int64(110*inputs+35*outputs) * (2 + w.rnd.Int63n(20))
}

func (w *world) withdraw(ex, to string, sats int64, t time.Time) {
	platformFee := int64(0)
	if ex == coinbase {
		platformFee = 1_000 + w.rnd.Int63n(2_000)
	}
	// The exchange's fee comes out of the balance too; never take more.
	sats = min(sats, w.bal[ex]-platformFee)
	if sats <= 0 {
		return
	}
	txid, made := w.tx(t, nil, 1, []out{{to, sats}, {"", 1_000_000 + w.rnd.Int63n(9_000_000)}})
	w.event(ex, importers.Event{Kind: importers.Withdrawal, Time: t.Add(-10 * time.Minute), Sats: sats, FeeSats: platformFee,
		Txid: txid, Address: made[0].addr})
}

// pay sends an amount outside from one or more wallets, with change back to
// the last. amount 0 means "most of what they hold".
func (w *world) pay(t time.Time, from []string, amount int64) {
	var ins []utxo
	var have int64
	for _, name := range from {
		if amount == 0 {
			got, s := w.pick(name, w.total(name)*3/4)
			ins, have = append(ins, got...), have+s
		} else {
			got, s := w.pick(name, amount-have+50_000) // room for the fee
			ins, have = append(ins, got...), have+s
		}
	}
	if amount == 0 {
		amount = have * 4 / 5
	}
	f := w.fee(len(ins), 2)
	change := have - amount - f
	if len(ins) == 0 || change < 1_000 {
		return
	}
	// Change goes back to the last wallet that paid in (savings, when the
	// phone can't cover it alone).
	txid, _ := w.tx(t, ins, 0, []out{{"", amount}, {from[len(from)-1], change}})
	// Most payments are labeled; some are left for the Review queue.
	if w.rnd.Intn(5) != 0 {
		w.labels = append(w.labels, trace.Label{Key: fmt.Sprintf("tx:%s:0", txid), Category: trace.Spend})
	}
}

func (w *world) sweep(from, to string, t time.Time) {
	ins := append([]utxo(nil), w.utxos[from]...)
	// Moving to another wallet takes even a single coin; consolidating a
	// wallet into itself needs two.
	if len(ins) == 0 || from == to && len(ins) < 2 {
		return
	}
	var have int64
	for _, u := range ins {
		have += u.sats
	}
	w.tx(t, ins, 0, []out{{to, have - w.fee(len(ins), 1)}})
}

// send moves an amount between two of the user's wallets, with change back.
func (w *world) send(from, to string, amount int64, t time.Time) {
	ins, sum := w.pick(from, amount+20_000)
	f := w.fee(len(ins), 2)
	if len(ins) == 0 || sum-amount-f < 1_000 {
		return
	}
	w.tx(t, ins, 0, []out{{to, amount}, {from, sum - amount - f}})
}

// sell sells some of an exchange balance at the day's price.
func (w *world) sell(ex string, sats int64, t time.Time) {
	cents := sats * w.priceOn(t) / 100_000_000
	w.event(ex, importers.Event{Kind: importers.Sell, Time: t, Sats: sats, FiatCents: cents, FeeCents: cents / 200, Currency: "USD"})
}

func (w *world) depositAndSell(from, ex string, t time.Time) {
	have := w.total(from)
	if have < 2_000_000 {
		return
	}
	amount := have / 6
	ins, sum := w.pick(from, amount+10_000)
	f := w.fee(len(ins), 2)
	if sum-amount-f < 1_000 {
		return
	}
	txid, made := w.tx(t, ins, 0, []out{{"", amount}, {from, sum - amount - f}})
	w.event(ex, importers.Event{Kind: importers.Deposit, Time: t.Add(40 * time.Minute), Sats: amount, Txid: txid, Address: made[0].addr})
	w.sell(ex, amount, t.Add(3*time.Hour))
}
