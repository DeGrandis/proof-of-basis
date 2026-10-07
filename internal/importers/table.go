package importers

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Table is a CSV with its header located (exports often have preamble lines).
type Table struct {
	Header   []string
	Rows     [][]string
	FirstRow int // 1-based line number of Rows[0]
	index    map[string]int
}

func norm(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "\ufeff")))
	return strings.Join(strings.Fields(s), " ")
}

// ReadTable parses a CSV and locates its header row.
func ReadTable(r io.Reader) (*Table, error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(bytes.NewReader(b))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	all, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("reading CSV: %w", err)
	}
	filled := func(row []string) int {
		n := 0
		for _, c := range row {
			if strings.TrimSpace(c) != "" {
				n++
			}
		}
		return n
	}
	// The header is the first of the leading rows with the most filled
	// cells; preamble lines ("Transactions", "User,...") have fewer.
	most := 0
	for i := 0; i < len(all) && i < 30; i++ {
		most = max(most, filled(all[i]))
	}
	for i, row := range all {
		if most < 3 || filled(row) != most {
			continue
		}
		t := &Table{Header: row, FirstRow: i + 2, index: map[string]int{}}
		for j, h := range row {
			if _, dup := t.index[norm(h)]; !dup {
				t.index[norm(h)] = j
			}
		}
		for _, rr := range all[i+1:] {
			empty := true
			for _, c := range rr {
				if strings.TrimSpace(c) != "" {
					empty = false
				}
			}
			if !empty {
				t.Rows = append(t.Rows, rr)
			}
		}
		return t, nil
	}
	return nil, errors.New("no header row found")
}

// Col returns the index of the first header matching any alias, or -1.
func (t *Table) Col(aliases ...string) int {
	for _, a := range aliases {
		if i, ok := t.index[norm(a)]; ok {
			return i
		}
	}
	return -1
}

// Has reports how many of the alias groups are present.
func (t *Table) Has(groups ...[]string) int {
	n := 0
	for _, g := range groups {
		if t.Col(g...) >= 0 {
			n++
		}
	}
	return n
}

func cell(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func (t *Table) RawRow(row []string) map[string]string {
	m := make(map[string]string, len(t.Header))
	for i, h := range t.Header {
		m[strings.TrimSpace(h)] = cell(row, i)
	}
	return m
}

var numClean = strings.NewReplacer("$", "", ",", "", " ", "", "USD", "", "BTC", "", "usd", "", "btc", "", " ", "")

// parseDecimal parses "1,234.5678", "$-12.00", "(3.50)" into a rational.
func parseDecimal(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return new(big.Rat), true
	}
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, s[1:len(s)-1]
	}
	s = numClean.Replace(s)
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, false
	}
	if neg {
		r.Neg(r)
	}
	return r, true
}

func scaled(s string, scale int64) (int64, error) {
	r, ok := parseDecimal(s)
	if !ok {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt64(scale))
	// round half away from zero
	n, d := r.Num(), r.Denom()
	q, m := new(big.Int).QuoRem(n, d, new(big.Int))
	if new(big.Int).Mul(new(big.Int).Abs(m), big.NewInt(2)).Cmp(d) >= 0 {
		if n.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q.Int64(), nil
}

// Sats parses a BTC decimal amount into signed satoshis.
func Sats(s string) (int64, error) { return scaled(s, 100_000_000) }

// Cents parses a fiat amount into signed cents.
func Cents(s string) (int64, error) { return scaled(s, 100) }

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00", // Fold: 2026-01-06 15:42:26.864543+00:00
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"01/02/2006 15:04:05",
	"01/02/2006 15:04",
	"1/2/2006 15:04:05",
	"1/2/2006 15:04",
	"1/2/2006 3:04:05 PM",
	"1/2/2006 3:04 PM",
	"Jan 2 2006 15:04:05",
	"Jan 02 2006 15:04:05",
	"Jan 2, 2006 15:04:05",
	"Jan 2, 2006 3:04 PM",
	"2006-01-02",
	"01/02/2006",
	"1/2/2006",
	"Jan 2, 2006",
}

// ParseTime accepts the formats seen in exchange exports. Times without a
// zone are taken as UTC (exports document UTC; the UI lets users shift a
// whole batch if theirs did not).
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), " UTC"))
	s = strings.TrimSuffix(s, " GMT")
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised date %q", s)
}

var (
	txidRe = regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`)
	addrRe = regexp.MustCompile(`\b(bc1[02-9ac-hj-np-z]{11,71}|[13][1-9A-HJ-NP-Za-km-z]{25,34}|tb1[02-9ac-hj-np-z]{11,71}|bcrt1[02-9ac-hj-np-z]{11,71})\b`)
)

// findTxid and findAddress pull identifiers out of free-text note columns.
func findTxid(s ...string) string {
	for _, x := range s {
		if m := txidRe.FindString(x); m != "" {
			return strings.ToLower(m)
		}
	}
	return ""
}

func findAddress(s ...string) string {
	for _, x := range s {
		if m := addrRe.FindString(x); m != "" {
			return m
		}
	}
	return ""
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func filledCells(row []string) int {
	n := 0
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			n++
		}
	}
	return n
}

func isTxid(s string) bool { return len(s) == 64 && txidRe.MatchString(s) }
