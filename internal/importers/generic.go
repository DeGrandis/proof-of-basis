package importers

import (
	"fmt"
	"strings"
)

// signedSpec describes an export that can be read as signed BTC and USD
// columns. Strike and Fold use it; more platforms can be added as data.
type signedSpec struct {
	id, name string
	// signature columns: all must be present for a confident Detect.
	signature [][]string
	date      []string
	timeCol   []string // optional separate time-of-day column
	typ       []string
	btc       []string // signed BTC amount, or unsigned with direction from typ
	usd       []string
	feeUSD    []string
	feeBTC    []string
	txid      []string
	addr      []string
	ref       []string
	note      []string
	status    []string // rows not completed are skipped
	// pair columns ("Amount 1","Currency 1") used when there is no btc/usd column
	pairs [][2][]string
	// fee pairs ("Fee 1" in "Currency 1")
	feePairs [][2][]string
	// outflow words force a negative sign when amounts are unsigned
	outWords  []string
	feeInFiat bool
}

func (s signedSpec) ID() string   { return s.id }
func (s signedSpec) Name() string { return s.name }

func (s signedSpec) Detect(t *Table) int {
	if len(s.signature) == 0 {
		return 0
	}
	return t.Has(s.signature...) * 100 / len(s.signature)
}

func (s signedSpec) Parse(t *Table) ([]Event, []Warning, error) {
	ix := func(a []string) int { return t.Col(a...) }
	iDate, iTime := ix(s.date), ix(s.timeCol)
	if iDate < 0 {
		return nil, nil, fmt.Errorf("%s: no date column", s.id)
	}
	var out []Event
	var warns []Warning
	for n, row := range t.Rows {
		line := t.FirstRow + n
		w := func(f string, a ...any) { warns = append(warns, Warning{line, fmt.Sprintf(f, a...)}) }
		if st := strings.ToLower(cell(row, ix(s.status)) + " " + cell(row, ix(s.typ))); strings.Contains(st, "fail") || strings.Contains(st, "cancel") || strings.Contains(st, "pending") || strings.Contains(st, "reversed") {
			continue
		}
		when := cell(row, iDate)
		if iTime >= 0 {
			when += " " + cell(row, iTime)
		}
		ts, err := ParseTime(when)
		if err != nil {
			w("%v", err)
			continue
		}
		typ := cell(row, ix(s.typ))
		r := signedRow{row: line, typ: typ, feeInFiat: s.feeInFiat,
			txid: strings.ToLower(cell(row, ix(s.txid))), addr: cell(row, ix(s.addr)),
			ref: cell(row, ix(s.ref)), note: cell(row, ix(s.note))}
		var perr error
		num := func(col []string, f func(string) (int64, error)) int64 {
			v, err := f(cell(row, ix(col)))
			if err != nil && perr == nil {
				perr = err
			}
			return v
		}
		if ix(s.btc) >= 0 || ix(s.usd) >= 0 {
			r.btc, r.fiat = num(s.btc, Sats), num(s.usd, Cents)
		}
		for _, p := range s.pairs {
			amt, ccy := cell(row, ix(p[0])), strings.ToUpper(cell(row, ix(p[1])))
			switch ccy {
			case "BTC":
				r.btc += num(p[0], Sats)
			case "USD":
				r.fiat += num(p[0], Cents)
			case "":
			default:
				if amt != "" {
					w("unsupported currency %q", ccy)
				}
			}
		}
		r.feeFiat, r.feeSats = abs(num(s.feeUSD, Cents)), abs(num(s.feeBTC, Sats))
		for _, p := range s.feePairs {
			switch strings.ToUpper(cell(row, ix(p[1]))) {
			case "BTC":
				r.feeSats += abs(num(p[0], Sats))
			case "USD":
				r.feeFiat += abs(num(p[0], Cents))
			}
		}
		if perr != nil {
			w("%v", perr)
			continue
		}
		// Unsigned exports: derive direction from the type text.
		if r.btc > 0 && r.fiat > 0 {
			if hasAny(typ, []string{"buy", "purchase"}) {
				r.fiat = -r.fiat
			} else if hasAny(typ, []string{"sell", "sale"}) {
				r.btc = -r.btc
			}
		}
		if r.btc > 0 && r.fiat == 0 && hasAny(typ, s.outWords) {
			r.btc = -r.btc
		}
		if r.txid == "" {
			r.txid = findTxid(r.note, typ)
		}
		if r.addr == "" {
			r.addr = findAddress(r.note)
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

var outflowWords = []string{"withdraw", "send", "sent", "payment", "pay ", "spend", "outgoing"}

func init() {
	// Strike transaction history export. Column names follow the
	// "Amount 1 / Currency 1 / Amount 2 / Currency 2" layout; a flat
	// "Amount BTC / Amount USD" layout is also accepted. Needs checking
	// against a real header line.
	Register(signedSpec{
		id: "strike", name: "Strike",
		signature: [][]string{{"Transaction ID", "Reference"}, {"Transaction Type"}, {"Amount 1", "Amount BTC"}, {"Currency 1", "Amount USD"}, {"BTC Price"}},
		date:      []string{"Completed Date (UTC)", "Time (UTC)", "Date & Time (UTC)", "Date (UTC)", "Date"},
		timeCol:   []string{"Completed Time (UTC)"},
		typ:       []string{"Transaction Type", "Type"},
		btc:       []string{"Amount BTC"},
		usd:       []string{"Amount USD"},
		feeUSD:    []string{"Fee USD"},
		feeBTC:    []string{"Fee BTC"},
		pairs:     [][2][]string{{{"Amount 1"}, {"Currency 1"}}, {{"Amount 2"}, {"Currency 2"}}},
		feePairs:  [][2][]string{{{"Fee 1"}, {"Currency 1"}}, {{"Fee 2"}, {"Currency 2"}}},
		txid:      []string{"Transaction Hash", "On-chain Transaction ID", "Txid"},
		addr:      []string{"Destination", "Address"},
		ref:       []string{"Transaction ID", "Reference"},
		note:      []string{"Description", "Note"},
		status:    []string{"Status", "State"},
		outWords:  outflowWords,
	})
	// Fold bitcoin transaction history. Exact export columns unknown; these
	// aliases cover the common shapes and must be checked against a header.
	Register(signedSpec{
		id: "fold", name: "Fold",
		signature: [][]string{{"Transaction Type", "Type"}, {"Amount (BTC)", "BTC Amount", "Quantity (BTC)"}, {"Amount (USD)", "USD Amount", "Subtotal (USD)", "Total (USD)"}, {"Price per Coin (USD)", "BTC Price", "Price (USD)"}},
		date:      []string{"Date (UTC)", "Date", "Timestamp", "Transaction Date"},
		typ:       []string{"Transaction Type", "Type", "Description"},
		btc:       []string{"Amount (BTC)", "BTC Amount", "Quantity (BTC)"},
		usd:       []string{"Amount (USD)", "USD Amount", "Subtotal (USD)", "Total (USD)"},
		feeUSD:    []string{"Fee (USD)", "Fees (USD)"},
		feeBTC:    []string{"Fee (BTC)", "Network Fee (BTC)"},
		txid:      []string{"Transaction Hash", "Txid", "Blockchain Transaction ID"},
		addr:      []string{"Destination", "Address", "Destination Address"},
		ref:       []string{"Transaction ID", "ID", "Reference"},
		note:      []string{"Description", "Note", "Memo"},
		status:    []string{"Status", "State"},
		outWords:  outflowWords,
	})
}
