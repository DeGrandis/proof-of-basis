package importers

import (
	"fmt"
	"strings"
)

// signedSpec describes an export that reads as a BTC column and a USD
// column (signed or not) plus fees. Strike and Fold use it; another
// platform with this shape is a new spec, not new code.
type signedSpec struct {
	id, name string
	// signature columns: the share present is the Detect score.
	signature [][]string
	date      []string
	typ       []string
	asset     []string // optional; rows for other assets are skipped
	btc       []string
	usd       []string
	price     []string // BTC price, used for reward value when USD is empty
	feeUSD    []string
	feeBTC    []string
	txid      []string
	addr      []string
	ref       []string
	note      []string
	status    []string // rows not completed are skipped
	// outWords mark BTC outflows when amounts are unsigned.
	outWords []string
	// feeInFiat: the USD column already includes the fee.
	feeInFiat bool
	// netReversals drops a transaction and its reversal (same reference,
	// amounts cancelling out).
	netReversals bool
}

func (s signedSpec) ID() string   { return s.id }
func (s signedSpec) Name() string { return s.name }

func (s signedSpec) Detect(t *Table) int {
	return t.Has(s.signature...) * 100 / len(s.signature)
}

type parsedRow struct {
	ev        Event
	ref       string
	btc, fiat int64
}

func (s signedSpec) Parse(t *Table) ([]Event, []Warning, error) {
	ix := func(a []string) int { return t.Col(a...) }
	iDate := ix(s.date)
	if iDate < 0 {
		return nil, nil, fmt.Errorf("%s: no date column", s.id)
	}
	// An export that has any negative BTC amount is signed; only unsigned
	// exports take their direction from the type text.
	signed := false
	if i := ix(s.btc); i >= 0 {
		for _, row := range t.Rows {
			if strings.HasPrefix(cell(row, i), "-") || strings.HasPrefix(cell(row, i), "(") {
				signed = true
				break
			}
		}
	}
	var rows []parsedRow
	var warns []Warning
	for n, row := range t.Rows {
		line := t.FirstRow + n
		w := func(f string, a ...any) { warns = append(warns, Warning{line, fmt.Sprintf(f, a...)}) }
		if filledCells(row) <= 1 || cell(row, iDate) == "" {
			continue // footer disclaimers and spacer rows
		}
		typ := cell(row, ix(s.typ))
		if st := strings.ToLower(cell(row, ix(s.status))); strings.Contains(st, "fail") || strings.Contains(st, "cancel") || strings.Contains(st, "pending") || strings.Contains(st, "reversed") {
			continue
		}
		if a := cell(row, ix(s.asset)); a != "" && !strings.EqualFold(a, "BTC") {
			continue
		}
		ts, err := ParseTime(cell(row, iDate))
		if err != nil {
			w("%v", err)
			continue
		}
		r := signedRow{row: line, typ: typ, feeInFiat: s.feeInFiat,
			addr: cell(row, ix(s.addr)), ref: cell(row, ix(s.ref)), note: cell(row, ix(s.note))}
		if tx := strings.ToLower(cell(row, ix(s.txid))); isTxid(tx) {
			r.txid = tx
		}
		var perr error
		num := func(col []string, f func(string) (int64, error)) int64 {
			v, err := f(cell(row, ix(col)))
			if err != nil && perr == nil {
				perr = err
			}
			return v
		}
		r.btc, r.fiat = num(s.btc, Sats), num(s.usd, Cents)
		r.feeFiat, r.feeSats = abs(num(s.feeUSD, Cents)), abs(num(s.feeBTC, Sats))
		price := abs(num(s.price, Cents))
		if perr != nil {
			w("%v", perr)
			continue
		}
		// Unsigned exports: take the direction from the type text.
		lt := strings.ToLower(typ)
		if !signed && r.btc > 0 && r.fiat > 0 {
			if hasAny(lt, []string{"buy", "purchase"}) {
				r.fiat = -r.fiat
			} else if hasAny(lt, []string{"sell", "sale"}) {
				r.btc = -r.btc
			}
		}
		if !signed && r.btc > 0 && r.fiat <= 0 && hasAny(lt, s.outWords) {
			r.btc = -r.btc
		}
		if r.btc == 0 && r.fiat > 0 && hasAny(lt, s.outWords) {
			r.fiat = -r.fiat // fiat withdrawal
		}
		if r.txid == "" {
			r.txid = findTxid(r.note)
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
		if (ev.Kind == Reward || ev.Kind == Income) && ev.FiatCents == 0 && price > 0 {
			ev.FiatCents = mulRound(price, ev.Sats, 100_000_000)
		}
		ev.Time, ev.Raw = ts, t.RawRow(row)
		rows = append(rows, parsedRow{ev, r.ref, r.btc, r.fiat})
	}
	if s.netReversals {
		rows, warns = netReversals(rows, warns)
	}
	out := make([]Event, len(rows))
	for i, r := range rows {
		out[i] = r.ev
	}
	return out, warns, nil
}

// netReversals removes groups of rows sharing a reference whose BTC and
// USD amounts sum to zero: a transaction that was reversed.
func netReversals(rows []parsedRow, warns []Warning) ([]parsedRow, []Warning) {
	type sum struct{ btc, fiat, n int64 }
	sums := map[string]*sum{}
	for _, r := range rows {
		if r.ref == "" {
			continue
		}
		if sums[r.ref] == nil {
			sums[r.ref] = &sum{}
		}
		s := sums[r.ref]
		s.btc += r.btc
		s.fiat += r.fiat
		s.n++
	}
	var out []parsedRow
	for _, r := range rows {
		if s := sums[r.ref]; r.ref != "" && s.n > 1 && s.btc == 0 && s.fiat == 0 {
			warns = append(warns, Warning{r.ev.Row, fmt.Sprintf("reference %s was reversed; both rows ignored", r.ref)})
			continue
		}
		out = append(out, r)
	}
	return out, warns
}

var outflowWords = []string{"withdraw", "send", "sent", "payment", "spend", "outgoing"}

func init() {
	// Strike "Annual Transactions" / "Account statement" exports, and the
	// "Account report" (ReferenceId, Timestamp, TransactionType, FiatAmount,
	// FiatFee, BitcoinAmount, BitcoinFee, BitcoinPrice, CostBasisUsd,
	// Destination). Types: Credit / Strike Credit, Deposit, Withdrawal (USD
	// unless a BTC amount is given); Purchase, Sell; Send, Receive, Bitcoin
	// Withdrawal, Bitcoin Deposit (BTC); Referral (BTC income). Strike reuses
	// the reference for a transaction and its reversal.
	Register(signedSpec{
		id: "strike", name: "Strike",
		signature: [][]string{{"Reference", "ReferenceId"}, {"Date & Time (UTC)", "Timestamp"}, {"Transaction Type", "TransactionType"},
			{"Amount USD", "FiatAmount"}, {"Amount BTC", "BitcoinAmount"}, {"BTC Price", "BitcoinPrice"}, {"Cost Basis (USD)", "CostBasisUsd"}},
		date:         []string{"Date & Time (UTC)", "Timestamp"},
		typ:          []string{"Transaction Type", "TransactionType"},
		btc:          []string{"Amount BTC", "BitcoinAmount"},
		usd:          []string{"Amount USD", "FiatAmount"},
		price:        []string{"BTC Price", "BitcoinPrice"},
		feeUSD:       []string{"Fee USD", "FiatFee"},
		feeBTC:       []string{"Fee BTC", "BitcoinFee"},
		txid:         []string{"Transaction Hash"},
		addr:         []string{"Destination"},
		ref:          []string{"Reference", "ReferenceId"},
		note:         []string{"Description", "Note"},
		outWords:     outflowWords,
		feeInFiat:    true, // Amount USD is the cash that moved, fee included
		netReversals: true,
	})
	// Fold bitcoin transaction history (fold-bitcoin-transaction-history-*.csv).
	// Metadata rows precede the header and disclaimers follow the data.
	// Amount (BTC) is unsigned; Total (USD) is signed and includes the fee.
	// Withdrawal and Deposit move BTC, not USD.
	Register(signedSpec{
		id: "fold", name: "Fold",
		signature: [][]string{{"Reference ID"}, {"Date (UTC)"}, {"Transaction Type"}, {"Amount (BTC)"},
			{"Price per Coin (USD)"}, {"Subtotal (USD)"}, {"Total (USD)"}},
		date:      []string{"Date (UTC)"},
		typ:       []string{"Transaction Type"},
		asset:     []string{"Asset"},
		btc:       []string{"Amount (BTC)"},
		usd:       []string{"Total (USD)"},
		price:     []string{"Price per Coin (USD)"},
		feeUSD:    []string{"Fee (USD)"},
		txid:      []string{"Transaction ID"},
		ref:       []string{"Reference ID"},
		note:      []string{"Description"},
		outWords:  outflowWords,
		feeInFiat: true,
	})
}
