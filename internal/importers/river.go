package importers

import (
	"fmt"
	"strings"
)

// River account activity export: one row per movement with sent/received
// amount and currency pairs, a fee pair, and a tag.
type river struct{}

func init() { Register(river{}) }

func (river) ID() string   { return "river" }
func (river) Name() string { return "River" }

var rvCols = struct{ date, sAmt, sCcy, rAmt, rCcy, fAmt, fCcy, tag, txid, addr, id []string }{
	date: []string{"Date", "Date (UTC)", "Timestamp"},
	sAmt: []string{"Sent Amount"},
	sCcy: []string{"Sent Currency"},
	rAmt: []string{"Received Amount"},
	rCcy: []string{"Received Currency"},
	fAmt: []string{"Fee Amount"},
	fCcy: []string{"Fee Currency"},
	tag:  []string{"Tag", "Type", "Description"},
	txid: []string{"Transaction Hash", "TxHash", "Txid", "Transaction ID"},
	addr: []string{"Address", "Destination", "Destination Address"},
	id:   []string{"ID", "Reference"},
}

func (river) Detect(t *Table) int {
	c := rvCols
	if t.Col(c.sAmt...) < 0 || t.Col(c.rAmt...) < 0 {
		return 0
	}
	return t.Has(c.date, c.sAmt, c.sCcy, c.rAmt, c.rCcy, c.fAmt, c.fCcy, c.tag) * 100 / 8
}

func (river) Parse(t *Table) ([]Event, []Warning, error) {
	c := rvCols
	ix := func(a []string) int { return t.Col(a...) }
	iDate := ix(c.date)
	if iDate < 0 {
		return nil, nil, fmt.Errorf("river: no Date column")
	}
	var out []Event
	var warns []Warning
	for n, row := range t.Rows {
		line := t.FirstRow + n
		w := func(f string, a ...any) { warns = append(warns, Warning{line, fmt.Sprintf(f, a...)}) }
		ts, err := ParseTime(cell(row, iDate))
		if err != nil {
			w("%v", err)
			continue
		}
		r := signedRow{row: line, typ: cell(row, ix(c.tag)), ref: cell(row, ix(c.id)),
			txid: strings.ToLower(cell(row, ix(c.txid))), addr: cell(row, ix(c.addr))}
		add := func(amt, ccy string, sign int64) error {
			if amt == "" {
				return nil
			}
			switch strings.ToUpper(ccy) {
			case "BTC":
				v, err := Sats(amt)
				r.btc += sign * abs(v)
				return err
			case "USD", "":
				v, err := Cents(amt)
				r.fiat += sign * abs(v)
				return err
			}
			return fmt.Errorf("unsupported currency %q", ccy)
		}
		if err := firstErr(add(cell(row, ix(c.sAmt)), cell(row, ix(c.sCcy)), -1),
			add(cell(row, ix(c.rAmt)), cell(row, ix(c.rCcy)), 1)); err != nil {
			w("%v", err)
			continue
		}
		if fa := cell(row, ix(c.fAmt)); fa != "" {
			if strings.EqualFold(cell(row, ix(c.fCcy)), "BTC") {
				v, err := Sats(fa)
				if err != nil {
					w("%v", err)
					continue
				}
				r.feeSats = abs(v)
			} else {
				v, err := Cents(fa)
				if err != nil {
					w("%v", err)
					continue
				}
				r.feeFiat = abs(v)
			}
		}
		// ASSUMPTION (verify against a real export header/sample): the sent
		// amount on a withdrawal includes the network fee, so the on-chain
		// output is sent minus fee. Matching tolerates either way.
		if r.btc < 0 && r.fiat == 0 && r.feeSats > 0 && -r.btc > r.feeSats {
			r.btc += r.feeSats
		}
		ev, ok, msg := classify(r)
		if msg != "" {
			w("%s", msg)
		}
		if !ok {
			continue
		}
		ev.Time, ev.Raw = ts, t.RawRow(row)
		out = append(out, ev)
	}
	return out, warns, nil
}
