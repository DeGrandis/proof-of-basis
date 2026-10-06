// Package importers turns exchange and bank CSV exports into normalised
// events. Handlers are pure: no database, no network.
package importers

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

type Kind string

const (
	Buy        Kind = "buy"        // fiat -> BTC on the platform
	Sell       Kind = "sell"       // BTC -> fiat on the platform
	Deposit    Kind = "deposit"    // BTC arrived from outside the platform
	Withdrawal Kind = "withdrawal" // BTC left the platform
	Income     Kind = "income"     // interest, staking, earn programs
	Reward     Kind = "reward"     // card cashback/rewards; tax treatment is the user's call
	FiatMove   Kind = "fiat"       // fiat deposit/withdrawal, informational only
)

// Event is one normalised row. Amounts are always positive; Kind gives the
// direction. FiatCents is the basis paid (including fees) for Buy and the
// gross proceeds for Sell, with FeeCents as the fee on top / taken out.
type Event struct {
	Kind      Kind
	Time      time.Time
	Sats      int64
	FiatCents int64
	FeeCents  int64
	FeeSats   int64 // BTC network or platform fee on a withdrawal
	Currency  string
	Txid      string
	Address   string
	Ref       string // the platform's own transaction id
	Note      string
	Row       int               // 1-based line in the file
	Raw       map[string]string // original row, kept for re-parsing and audit
}

type Warning struct {
	Row     int
	Message string
}

type Handler interface {
	ID() string
	Name() string
	// Detect scores how likely the file came from this platform (0..100).
	Detect(t *Table) int
	Parse(t *Table) ([]Event, []Warning, error)
}

var registry = map[string]Handler{}

func Register(h Handler) { registry[h.ID()] = h }

func Get(id string) (Handler, bool) { h, ok := registry[id]; return h, ok }

func All() []Handler {
	out := make([]Handler, 0, len(registry))
	for _, h := range registry {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

var ErrUnknownFormat = errors.New("could not recognise this CSV; pick the platform manually or use the column mapper")

// Import reads a CSV and parses it with the given handler, or the best
// detected one when id is empty.
func Import(r io.Reader, id string) (Handler, []Event, []Warning, error) {
	t, err := ReadTable(r)
	if err != nil {
		return nil, nil, nil, err
	}
	var h Handler
	if id != "" {
		var ok bool
		if h, ok = Get(id); !ok {
			return nil, nil, nil, fmt.Errorf("unknown handler %q", id)
		}
	} else {
		best := 0
		for _, c := range All() {
			if s := c.Detect(t); s > best {
				best, h = s, c
			}
		}
		if best < 50 {
			return nil, nil, nil, ErrUnknownFormat
		}
	}
	ev, warns, err := h.Parse(t)
	return h, ev, warns, err
}
