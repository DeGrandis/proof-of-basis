// Package prices provides daily BTC/USD closes. A history file is baked into
// the binary at build time; sources are pluggable and only contacted when
// the user (or the image build) explicitly asks for an update.
package prices

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source fetches daily closes in USD cents per BTC, keyed by YYYY-MM-DD (UTC).
type Source interface {
	ID() string
	Name() string
	Host() string // shown to the user before contacting it
	Fetch(ctx context.Context, from, to time.Time) (map[string]int64, error)
}

var registry = map[string]Source{}

func Register(s Source) { registry[s.ID()] = s }

func Get(id string) (Source, bool) { s, ok := registry[id]; return s, ok }

func All() []Source {
	out := make([]Source, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

//go:embed data/btcusd-daily.csv
var bundled string

// Bundled returns the price history compiled into this binary.
func Bundled() map[string]int64 {
	m, _ := ReadCSV(strings.NewReader(bundled))
	return m
}

// ReadCSV parses "date,close_usd" rows.
func ReadCSV(r io.Reader) (map[string]int64, error) {
	out := map[string]int64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		d, p, ok := strings.Cut(line, ",")
		if !ok || len(d) != 10 || d[4] != '-' {
			continue // header or junk
		}
		c, err := dollarsToCents(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d, err)
		}
		if c > 0 {
			out[d] = c
		}
	}
	return out, sc.Err()
}

// WriteCSV writes rows sorted by date.
func WriteCSV(w io.Writer, m map[string]int64) error {
	days := make([]string, 0, len(m))
	for d := range m {
		days = append(days, d)
	}
	sort.Strings(days)
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "date,close_usd")
	for _, d := range days {
		fmt.Fprintf(bw, "%s,%d.%02d\n", d, m[d]/100, m[d]%100)
	}
	return bw.Flush()
}

func dollarsToCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt((frac + "000")[:3], 10, 64)
	if err1 != nil || err2 != nil || w < 0 {
		return 0, fmt.Errorf("bad price %q", s)
	}
	return w*100 + (f+5)/10, nil
}

func floatToCents(v float64) int64 { return int64(v*100 + 0.5) }

var client = &http.Client{Timeout: 60 * time.Second}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Yahoo rejects requests without a browser-like agent. No cookies,
	// no identifiers are sent.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; btctax price history)")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d %s", req.URL.Host, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}
