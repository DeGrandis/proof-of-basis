package importers

import (
	"fmt"
	"math/big"
	"strings"
)

// Coinbase "Transaction history" CSV (coinbase.com > Reports). Older exports
// call the price column "Spot Price at Transaction"; newer ones add an ID
// column and may sign quantities. Only BTC rows are imported.
type coinbase struct{}

func init() { Register(coinbase{}) }

func (coinbase) ID() string   { return "coinbase" }
func (coinbase) Name() string { return "Coinbase" }

var cbCols = struct{ id, time, typ, asset, qty, ccy, price, sub, total, fees, notes []string }{
	id:    []string{"ID"},
	time:  []string{"Timestamp"},
	typ:   []string{"Transaction Type"},
	asset: []string{"Asset"},
	qty:   []string{"Quantity Transacted"},
	ccy:   []string{"Price Currency", "Spot Price Currency"},
	price: []string{"Price at Transaction", "Spot Price at Transaction"},
	sub:   []string{"Subtotal"},
	total: []string{"Total (inclusive of fees and/or spread)", "Total (inclusive of fees)", "Total"},
	fees:  []string{"Fees and/or Spread", "Fees"},
	notes: []string{"Notes"},
}

func (coinbase) Detect(t *Table) int {
	c := cbCols
	n := t.Has(c.time, c.typ, c.asset, c.qty, c.sub, c.fees)
	if t.Col(c.qty...) < 0 {
		return 0
	}
	return n * 100 / 6
}

func (coinbase) Parse(t *Table) ([]Event, []Warning, error) {
	c := cbCols
	ix := func(a []string) int { return t.Col(a...) }
	iTime, iTyp, iAsset, iQty := ix(c.time), ix(c.typ), ix(c.asset), ix(c.qty)
	if iTime < 0 || iTyp < 0 || iAsset < 0 || iQty < 0 {
		return nil, nil, fmt.Errorf("coinbase: missing Timestamp, Transaction Type, Asset or Quantity Transacted column")
	}
	iID, iCcy, iPrice, iSub, iTotal, iFees, iNotes := ix(c.id), ix(c.ccy), ix(c.price), ix(c.sub), ix(c.total), ix(c.fees), ix(c.notes)
	var out []Event
	var warns []Warning
	for n, row := range t.Rows {
		line := t.FirstRow + n
		if !strings.EqualFold(cell(row, iAsset), "BTC") {
			continue
		}
		w := func(f string, a ...any) { warns = append(warns, Warning{line, fmt.Sprintf(f, a...)}) }
		ts, err := ParseTime(cell(row, iTime))
		if err != nil {
			w("%v", err)
			continue
		}
		if ccy := cell(row, iCcy); ccy != "" && !strings.EqualFold(ccy, "USD") {
			w("price currency is %s; only USD is supported so far", ccy)
			continue
		}
		qty, err1 := Sats(cell(row, iQty))
		sub, err2 := Cents(cell(row, iSub))
		total, err3 := Cents(cell(row, iTotal))
		fees, err4 := Cents(cell(row, iFees))
		if err := firstErr(err1, err2, err3, err4); err != nil {
			w("%v", err)
			continue
		}
		qty, sub, total, fees = abs(qty), abs(sub), abs(total), abs(fees)
		typ := cell(row, iTyp)
		notes := cell(row, iNotes)
		ev := Event{Time: ts, Sats: qty, Row: line, Ref: cell(row, iID), Note: notes, Currency: "USD", Raw: t.RawRow(row)}
		lt := strings.ToLower(typ)
		switch {
		case strings.Contains(lt, "buy"):
			ev.Kind, ev.FeeCents = Buy, fees
			ev.FiatCents = total // total includes fees: that is the basis
			if total == 0 {
				ev.FiatCents = sub + fees
			}
		case strings.Contains(lt, "sell"), lt == "convert":
			ev.Kind, ev.FeeCents = Sell, fees
			ev.FiatCents = sub
			if sub == 0 {
				ev.FiatCents = total + fees
			}
			if lt == "convert" {
				w("BTC converted to another asset; treated as a sale at the USD subtotal")
			}
		case lt == "send", strings.Contains(lt, "withdraw"):
			ev.Kind = Withdrawal
			ev.Address = findAddress(notes)
			ev.Txid = findTxid(notes)
		case lt == "receive", strings.Contains(lt, "deposit"):
			ev.Kind = Deposit
			ev.Txid = findTxid(notes)
			if hasAny(typ+" "+notes, incomeWords) {
				ev.Kind, ev.FiatCents = Income, sub
			}
		case hasAny(lt, incomeWords) || strings.Contains(lt, "reward"):
			ev.Kind, ev.FiatCents = Income, sub
			if sub == 0 {
				if p, err := Cents(cell(row, iPrice)); err == nil {
					ev.FiatCents = mulRound(p, qty, 100_000_000)
				}
			}
		default:
			w("unrecognised transaction type %q; skipped", typ)
			continue
		}
		out = append(out, ev)
	}
	return out, warns, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// mulRound returns a*b/c rounded half away from zero, without overflow.
func mulRound(a, b, c int64) int64 {
	r := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(a), big.NewInt(b)), big.NewInt(c))
	n, err := scaled(r.FloatString(4), 1)
	if err != nil {
		return 0
	}
	return n
}
