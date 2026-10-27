package importers

import (
	"fmt"
	"strings"
	"time"
)

// Cash App activity export (cash_app_report_*.csv): one row per payment,
// card purchase, transfer and bitcoin movement. Bitcoin rows have Asset
// Type BTC, the coins in Asset Amount (unsigned) and the price in Asset
// Price; Amount, Fee and Net Amount are signed USD. Types: Bitcoin Buy,
// Bitcoin Sale, Bitcoin Withdrawal, Bitcoin Deposit, Bitcoin Boost (card
// reward). Rows that are only cash (payments, card spending, cash in and
// out) are skipped. Dates end in a US zone abbreviation (EST, PDT).
type cashapp struct{}

func init() { Register(cashapp{}) }

func (cashapp) ID() string   { return "cashapp" }
func (cashapp) Name() string { return "Cash App" }

var caCols = struct{ id, date, typ, amt, fee, net, asset, price, qty, status, notes []string }{
	id:     []string{"Transaction ID"},
	date:   []string{"Date"},
	typ:    []string{"Transaction Type"},
	amt:    []string{"Amount"},
	fee:    []string{"Fee"},
	net:    []string{"Net Amount"},
	asset:  []string{"Asset Type"},
	price:  []string{"Asset Price"},
	qty:    []string{"Asset Amount"},
	status: []string{"Status"},
	notes:  []string{"Notes"},
}

func (cashapp) Detect(t *Table) int {
	c := caCols
	if t.Col(c.qty...) < 0 || t.Col(c.asset...) < 0 {
		return 0
	}
	return t.Has(c.id, c.date, c.typ, c.amt, c.fee, c.net, c.asset, c.price, c.qty, c.status) * 100 / 10
}

func (cashapp) Parse(t *Table) ([]Event, []Warning, error) {
	c := caCols
	ix := func(a []string) int { return t.Col(a...) }
	iDate, iTyp, iQty := ix(c.date), ix(c.typ), ix(c.qty)
	if iDate < 0 || iTyp < 0 || iQty < 0 {
		return nil, nil, fmt.Errorf("cashapp: missing Date, Transaction Type or Asset Amount column")
	}
	iID, iAmt, iFee, iNet, iAsset, iPrice, iStatus, iNotes := ix(c.id), ix(c.amt), ix(c.fee), ix(c.net), ix(c.asset), ix(c.price), ix(c.status), ix(c.notes)
	var out []Event
	var warns []Warning
	for n, row := range t.Rows {
		line := t.FirstRow + n
		typ, qty := cell(row, iTyp), cell(row, iQty)
		lt := strings.ToLower(typ)
		asset := cell(row, iAsset)
		if !strings.EqualFold(asset, "BTC") && !(asset == "" && qty != "" && strings.Contains(lt, "bitcoin")) {
			continue // cash only, or another asset
		}
		if st := strings.ToLower(cell(row, iStatus)); hasAny(st, []string{"fail", "cancel", "declin", "pending", "refund", "revers"}) {
			continue
		}
		w := func(f string, a ...any) { warns = append(warns, Warning{line, fmt.Sprintf(f, a...)}) }
		ts, err := cashAppTime(cell(row, iDate))
		if err != nil {
			w("%v", err)
			continue
		}
		sats, err1 := Sats(qty)
		amt, err2 := Cents(cell(row, iAmt))
		fee, err3 := Cents(cell(row, iFee))
		net, err4 := Cents(cell(row, iNet))
		if err := firstErr(err1, err2, err3, err4); err != nil {
			w("%v", err)
			continue
		}
		sats, amt, fee, net = abs(sats), abs(amt), abs(fee), abs(net)
		// Which of Amount and Net Amount includes the fee is not
		// documented, but a fee only adds to what a buy cost and takes from
		// what a sale paid.
		paid, got := max(amt, net), min(amt, net)
		if cell(row, iNet) == "" {
			paid, got = amt+fee, amt
		}
		notes := cell(row, iNotes)
		ev := Event{Time: ts, Sats: sats, Row: line, Ref: cell(row, iID), Note: notes, Currency: "USD", Raw: t.RawRow(row)}
		value := amt
		if value == 0 {
			if p, err := Cents(cell(row, iPrice)); err == nil {
				value = mulRound(abs(p), sats, 100_000_000)
			}
		}
		switch {
		case hasAny(lt, []string{"sale", "sell", "sold"}):
			ev.Kind, ev.FiatCents, ev.FeeCents = Sell, got+fee, fee // gross proceeds
		case hasAny(lt, []string{"buy", "purchase", "round up"}):
			ev.Kind, ev.FiatCents, ev.FeeCents = Buy, paid, fee
		case hasAny(lt, rewardWords):
			ev.Kind, ev.FiatCents = Reward, value
		case hasAny(lt, incomeWords):
			ev.Kind, ev.FiatCents = Income, value
		case hasAny(lt, []string{"withdraw", "send", "sent"}):
			ev.Kind = Withdrawal
			ev.Address, ev.Txid = findAddress(notes), findTxid(notes)
			ev.FeeSats, ev.FeeApprox = usdFeeSats(fee, cell(row, iPrice))
		case hasAny(lt, []string{"deposit", "receive"}):
			ev.Kind, ev.FiatCents = Deposit, amt
			ev.Txid = findTxid(notes)
		default:
			w("unrecognised transaction type %q; skipped", typ)
			continue
		}
		out = append(out, ev)
	}
	return out, warns, nil
}

var usZones = map[string]string{"EST": "-0500", "EDT": "-0400", "CST": "-0600", "CDT": "-0500", "MST": "-0700", "MDT": "-0600",
	"PST": "-0800", "PDT": "-0700", "AKST": "-0900", "AKDT": "-0800", "HST": "-1000"}

// cashAppTime reads "2024-01-05 09:00:00 EST". Go takes a zone
// abbreviation it does not know as UTC, so they become offsets first.
func cashAppTime(s string) (time.Time, error) {
	if i := strings.LastIndexByte(s, ' '); i > 0 {
		if off, ok := usZones[strings.ToUpper(s[i+1:])]; ok {
			s = s[:i] + " " + off
		}
	}
	return ParseTime(s)
}
