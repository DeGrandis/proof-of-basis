// Package tax holds jurisdiction-specific rules. The lot engine only talks
// to the Rules interface, so other countries can be added later.
package tax

import "time"

type Method string

const (
	FIFO     Method = "fifo"
	LIFO     Method = "lifo"
	Specific Method = "specific" // specific identification; HIFO is a helper on top
)

type Rules interface {
	ID() string
	// LongTerm reports whether a unit held from acquired to disposed gets
	// long-term treatment.
	LongTerm(acquired, disposed time.Time) bool
	// Methods lists the lot-selection methods the jurisdiction allows.
	Methods() []Method
	// PerAccountFrom is when basis must be tracked per wallet/account
	// instead of universally. Zero means always per account.
	PerAccountFrom() time.Time
}

// US implements federal US rules for property (IRS Notice 2014-21).
type US struct{}

func (US) ID() string { return "us" }

// LongTerm: held more than one year. A unit sold on the anniversary of its
// purchase is still short-term; the day after is long-term.
func (US) LongTerm(acquired, disposed time.Time) bool {
	a := acquired.UTC()
	d := disposed.UTC()
	anniv := time.Date(a.Year()+1, a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	return day.After(anniv)
}

func (US) Methods() []Method { return []Method{FIFO, LIFO, Specific} }

// Rev. Proc. 2024-28: wallet-by-wallet basis tracking from 1 Jan 2025.
func (US) PerAccountFrom() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

func ByID(id string) (Rules, bool) {
	switch id {
	case "us", "":
		return US{}, true
	}
	return nil, false
}
