// Package demo fills an empty database with a large, invented history:
// years of buys on three exchanges, withdrawals to four wallets,
// consolidations, batch payments, spends funded by two wallets at once,
// deposits back and sales. It is for trying the app and testing the flow
// view at a realistic size. Nothing in it is real.
package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"btctax/internal/importers"
	"btctax/internal/prices"
	"btctax/internal/scan"
	"btctax/internal/store"
	"btctax/internal/trace"
)

const (
	coinbase, river, strike = "Coinbase", "River", "Strike"
	cold, hot, vault, spend = "Cold storage", "Hot wallet", "Vault (multisig)", "Spending"
)

var wallets = []string{cold, hot, vault, spend}

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
	for _, ex := range []string{coinbase, river, strike} {
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
	start := time.Date(2019, 1, 7, 15, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)
	week := 0
	for t := start; t.Before(end); t = t.AddDate(0, 0, 7) {
		week++
		// Weekly buys: Coinbase until 2023, River from 2021, Strike from 2023.
		if t.Year() < 2023 {
			w.buy(coinbase, t, 50+w.rnd.Int63n(150))
		}
		if t.Year() >= 2021 {
			w.buy(river, t.Add(2*time.Hour), 100+w.rnd.Int63n(100))
		}
		if t.Year() >= 2023 {
			w.buy(strike, t.Add(4*time.Hour), 25+w.rnd.Int63n(75))
		}
		if week%13 == 5 && t.Year() < 2023 {
			w.event(coinbase, importers.Event{Kind: importers.Income, Time: t.Add(time.Hour), Sats: 2_000 + w.rnd.Int63n(8_000), Note: "Earn reward"})
		}
		// Monthly withdrawals to cold storage, now and then to the hot wallet.
		if week%4 == 0 {
			for _, ex := range []string{coinbase, river, strike} {
				if w.bal[ex] > 300_000 {
					to := cold
					if w.rnd.Intn(4) == 0 {
						to = hot
					}
					w.withdraw(ex, to, w.bal[ex]-10_000-w.rnd.Int63n(50_000), t.Add(26*time.Hour))
				}
			}
		}
		// Everyday spending from the hot wallet and the spending wallet.
		if week%3 == 1 {
			w.pay(t.Add(30*time.Hour), []string{hot}, 20_000+w.rnd.Int63n(80_000))
		}
		if week%5 == 2 {
			w.pay(t.Add(50*time.Hour), []string{spend}, 10_000+w.rnd.Int63n(60_000))
		}
		// Many to one across wallets: hot and spending fund one payment.
		if week%17 == 9 {
			w.pay(t.Add(70*time.Hour), []string{hot, spend}, 0)
		}
		// Twice a year cold storage sweeps every coin into the vault: many
		// inputs, one output.
		if week%26 == 20 {
			w.sweep(cold, vault, t.Add(80*time.Hour))
		}
		// Once a year cold storage consolidates into itself first.
		if week%52 == 33 {
			w.sweep(cold, cold, t.Add(90*time.Hour))
		}
		// Quarterly, the vault pays out to the hot and spending wallets and a
		// supplier in one transaction: one input, many outputs.
		if week%13 == 7 {
			w.batch(vault, t.Add(100*time.Hour))
		}
		// Yearly, some coins go back to an exchange and are sold.
		if week%52 == 40 && t.Year() >= 2021 {
			w.depositAndSell(vault, river, t.Add(110*time.Hour))
		}
	}
}

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
	return fmt.Sprintf("bc1qdemo%s%06d", map[string]string{cold: "cold", hot: "hot", vault: "vault", spend: "spend"}[owner], w.n)
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
	txid, made := w.tx(t, nil, 1, []out{{to, sats}, {"", 1_000_000 + w.rnd.Int63n(9_000_000)}})
	platformFee := int64(0)
	if ex == coinbase {
		platformFee = 1_000 + w.rnd.Int63n(2_000)
	}
	w.event(ex, importers.Event{Kind: importers.Withdrawal, Time: t.Add(-10 * time.Minute), Sats: sats, FeeSats: platformFee,
		Txid: txid, Address: made[0].addr})
}

// pay sends an amount outside from one or more wallets, with change back to
// the first. amount 0 means "most of what they hold".
func (w *world) pay(t time.Time, from []string, amount int64) {
	var ins []utxo
	var have int64
	for _, name := range from {
		if amount == 0 {
			got, s := w.pick(name, w.total(name)*3/4)
			ins, have = append(ins, got...), have+s
		} else {
			got, s := w.pick(name, amount-have+5_000)
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
	txid, _ := w.tx(t, ins, 0, []out{{"", amount}, {from[0], change}})
	// Most payments are labeled; some are left for the Review queue.
	if w.rnd.Intn(5) != 0 {
		w.labels = append(w.labels, trace.Label{Key: fmt.Sprintf("tx:%s:0", txid), Category: trace.Spend})
	}
}

func (w *world) sweep(from, to string, t time.Time) {
	ins := append([]utxo(nil), w.utxos[from]...)
	if len(ins) < 2 {
		return
	}
	var have int64
	for _, u := range ins {
		have += u.sats
	}
	w.tx(t, ins, 0, []out{{to, have - w.fee(len(ins), 1)}})
}

func (w *world) batch(from string, t time.Time) {
	have := w.total(from)
	if have < 2_000_000 {
		return
	}
	// Pay out about a tenth of the vault; the rest comes back as change.
	a, b, c := have/25, have/30, have/40
	ins, sum := w.pick(from, a+b+c+50_000)
	f := w.fee(len(ins), 4)
	change := sum - a - b - c - f
	if change < 10_000 {
		return
	}
	txid, _ := w.tx(t, ins, 0, []out{{hot, a}, {spend, b}, {"", c}, {from, change}})
	w.labels = append(w.labels, trace.Label{Key: fmt.Sprintf("tx:%s:2", txid), Category: trace.Spend, Note: "Supplier invoice"})
}

func (w *world) depositAndSell(from, ex string, t time.Time) {
	have := w.total(from)
	if have < 2_000_000 {
		return
	}
	amount := have / 4
	ins, sum := w.pick(from, amount+10_000)
	f := w.fee(len(ins), 2)
	if sum-amount-f < 1_000 {
		return
	}
	txid, made := w.tx(t, ins, 0, []out{{"", amount}, {from, sum - amount - f}})
	w.event(ex, importers.Event{Kind: importers.Deposit, Time: t.Add(40 * time.Minute), Sats: amount, Txid: txid, Address: made[0].addr})
	sell := t.Add(3 * time.Hour)
	cents := amount * w.priceOn(sell) / 100_000_000
	w.event(ex, importers.Event{Kind: importers.Sell, Time: sell, Sats: amount, FiatCents: cents, FeeCents: cents / 200, Currency: "USD"})
}
