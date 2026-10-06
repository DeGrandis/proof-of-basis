package importers

import (
	"fmt"
	"strings"
)

// signedRow is the platform-neutral shape most exports reduce to: a signed
// BTC change and a signed fiat change on the account, plus fees.
type signedRow struct {
	row              int
	typ              string
	btc, fiat        int64 // signed, sats and cents
	feeFiat, feeSats int64 // positive
	feeInFiat        bool  // fiat amount already includes the fee
	txid, addr, ref  string
	note             string
}

var incomeWords = []string{"interest", "income", "staking", "earn", "yield", "bonus", "referral", "learning reward", "inflation reward"}
var rewardWords = []string{"reward", "cashback", "cash back", "spin", "boost"}

func hasAny(s string, words []string) bool {
	s = strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// classify turns a signed row into an event. ok is false for rows with no
// BTC movement that are not fiat transfers worth keeping.
func classify(r signedRow) (Event, bool, string) {
	ev := Event{Row: r.row, Txid: r.txid, Address: r.addr, Ref: r.ref, Note: r.note, Currency: "USD"}
	switch {
	case r.btc > 0 && r.fiat < 0:
		ev.Kind, ev.Sats = Buy, r.btc
		ev.FiatCents = -r.fiat
		if !r.feeInFiat {
			ev.FiatCents += r.feeFiat
		}
		ev.FeeCents = r.feeFiat
	case r.btc < 0 && r.fiat > 0:
		ev.Kind, ev.Sats = Sell, -r.btc
		// Proceeds are gross; the fee is subtracted by the engine.
		ev.FiatCents = r.fiat
		if r.feeInFiat {
			ev.FiatCents += r.feeFiat
		}
		ev.FeeCents = r.feeFiat
	case r.btc < 0:
		ev.Kind, ev.Sats, ev.FeeSats = Withdrawal, -r.btc, r.feeSats
	case r.btc > 0:
		ev.Kind, ev.Sats = Deposit, r.btc
		switch {
		case hasAny(r.typ, rewardWords):
			ev.Kind = Reward
		case hasAny(r.typ, incomeWords):
			ev.Kind = Income
		}
		if r.fiat > 0 {
			ev.FiatCents = r.fiat // FMV when the export provides it
		}
	case r.fiat != 0:
		ev.Kind, ev.FiatCents = FiatMove, abs(r.fiat)
	default:
		return ev, false, ""
	}
	if r.btc != 0 && r.fiat != 0 && (r.btc > 0) == (r.fiat > 0) && ev.Kind != Deposit && ev.Kind != Income && ev.Kind != Reward {
		return ev, true, fmt.Sprintf("BTC and USD moved the same direction (%q); check this row", r.typ)
	}
	return ev, true, ""
}
