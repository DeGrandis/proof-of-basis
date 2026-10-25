// Package importers turns exchange and bank CSV exports into normalised
// events. Handlers are pure: no database, no network.
package importers

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
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
	// FeeApprox: FeeSats was converted from a fee the export gives only in
	// USD, so it is near but not exact, and may sit inside the amount.
	FeeApprox bool
	Currency  string
	Txid      string
	Address   string
	Ref       string // the platform's own transaction id
	// UID identifies the row across overlapping exports so re-importing a
	// longer history never duplicates it: the platform id when there is one,
	// otherwise a hash of the row's contents.
	UID  string
	Note string
	Row  int               // 1-based line in the file
	Raw  map[string]string // original row, kept for re-parsing and audit
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
	if err != nil {
		return h, nil, warns, err
	}
	assignUIDs(h.ID(), t, ev)
	return h, ev, warns, nil
}

// canonicalCell normalises a cell for hashing: numbers to a fixed form,
// dates to Unix seconds, text trimmed.
func canonicalCell(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if r, ok := parseDecimal(s); ok && strings.ContainsAny(s, "0123456789") && !strings.ContainsAny(s, ":/") {
		return "n:" + r.FloatString(12)
	}
	if ts, err := ParseTime(s); err == nil {
		return "t:" + strconv.FormatInt(ts.Unix(), 10)
	}
	return "s:" + s
}

func assignUIDs(platform string, t *Table, evs []Event) {
	seen := map[string]int{}
	for i := range evs {
		e := &evs[i]
		if e.UID != "" {
			continue
		}
		base := ""
		if e.Ref != "" {
			base = platform + ":" + e.Ref
		} else {
			// Hash the row's meaning, not its text: "0.0059" and "0.00590000"
			// are the same amount, and a date is the same instant however it
			// is written, so a re-export with new formatting never duplicates.
			h := sha256.New()
			for _, col := range t.Header {
				fmt.Fprintf(h, "%s\x1f", canonicalCell(e.Raw[strings.TrimSpace(col)]))
			}
			base = fmt.Sprintf("%s:row-%x", platform, h.Sum(nil)[:12])
		}
		// Platforms reuse ids (Strike: a transaction and its reversal), and
		// identical rows can legitimately repeat; number the repeats.
		seen[base]++
		e.UID = base
		if seen[base] > 1 {
			e.UID = fmt.Sprintf("%s#%d", base, seen[base])
		}
	}
}
