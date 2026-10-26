package app

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Readiness answers "is this year ready to hand to a tax professional?":
// what is complete, and what to fix first, with where to fix it.

type Check struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok, warn, bad
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Link   string `json:"link,omitempty"` // a page in the app
}

type Readiness struct {
	Year int `json:"year"`
	// KnownBasis is the share of BTC sold or spent that year whose
	// purchase is known, in basis points.
	KnownBasis int     `json:"knownBasisBP"`
	Ready      bool    `json:"ready"`
	Checks     []Check `json:"checks"`
	// When the user marked the year's numbers reviewed and last
	// downloaded its package (RFC 3339), for the tax season steps.
	Reviewed   string `json:"reviewed,omitempty"`
	Downloaded string `json:"downloaded,omitempty"`
}

func (s *Server) readiness(rep *Report, year int) (*Readiness, error) {
	z := loc(rep.Settings)
	start := time.Date(year, 1, 1, 0, 0, 0, 0, z)
	end := start.AddDate(1, 0, 0)
	in := func(t time.Time) bool { return !t.Before(start) && t.Before(end) }
	rd := &Readiness{Year: year, Checks: []Check{}}
	add := func(id, status, title, detail, link string) {
		rd.Checks = append(rd.Checks, Check{id, status, title, detail, link})
	}

	// Sales and spends with no known purchase.
	var sold, missing int64
	for _, d := range rep.Disposals {
		if in(d.Disposed) {
			sold += d.Sats
			if d.Missing {
				missing += d.Sats
			}
		}
	}
	rd.KnownBasis = 10000
	if sold > 0 {
		rd.KnownBasis = int((sold - missing) * 10000 / sold)
	}
	if missing > 0 {
		add("basis", "bad", fmt.Sprintf("%s BTC sold with no known purchase", satsStr(missing)),
			"Counted at $0 basis. Import or label the source.", "#taxes")
	} else if sold > 0 {
		add("basis", "ok", "Every sale has a known purchase", "", "")
	}

	// Review items in the year.
	var review int
	for _, it := range rep.Unmatched {
		if in(it.Time) {
			review++
		}
	}
	if review > 0 {
		add("review", "bad", fmt.Sprintf("%d unlabeled record%s in %d", review, plural(review), year),
			"Not counted until labeled.", "#review")
	} else {
		add("review", "ok", "Nothing waiting in Review", "", "")
	}

	// Accounts that went below zero by the end of the year.
	var dips []string
	for _, d := range rep.Dips {
		if time.Unix(d.T, 0).Before(end) {
			dips = append(dips, d.Account)
		}
	}
	if len(dips) > 0 {
		add("dips", "bad", "Below zero: "+strings.Join(dips, ", "),
			"Coins spent that never arrived. See Overview.", "#overview")
	}

	// Values with no price.
	var noPrice int
	for _, g := range rep.Graph {
		if strings.HasPrefix(g.ValueSource, "no price") && in(time.Unix(g.Time, 0)) {
			noPrice++
		}
	}
	if noPrice > 0 {
		add("prices", "bad", fmt.Sprintf("%d record%s with no price", noPrice, plural(noPrice)),
			"Update prices, or enter the value.", "#settings")
	}

	// Form 8949 against the 1099-DAs.
	f, err := s.forms(rep, year)
	if err != nil {
		return nil, err
	}
	if year >= 2025 {
		for _, c := range f.Checks {
			id := "1099da:" + c.Account
			switch c.Status {
			case "no form":
				add(id, "warn", "No 1099-DA from "+c.Account,
					"Add the PDF and its total.", "#taxes")
			case "no totals":
				add(id, "warn", "Enter "+c.Account+"'s 1099-DA total",
					"It's checked against your records.", "#taxes")
			case "differs":
				add(id, "bad", fmt.Sprintf("%s's 1099-DA: %s; your records: %s", c.Account, usdText(*c.FormProceeds), usdText(c.Proceeds)),
					fmt.Sprintf("Form is %s than your records. Likely a missing import, a different date or amount, or a broker error.", moreLess(c.Diff)), "#taxes")
			case "match":
				add(id, "ok", c.Account+"'s 1099-DA matches", "", "")
			}
		}
		if n := f.Issues; n > 0 {
			add("reconcile", "warn", fmt.Sprintf("%d Form 8949 line%s to check", n, plural(n)),
				"Mismatches with 1099-DA lines, or unknown purchases.", "#taxes")
		}
	}

	// The January 2025 split.
	if year >= 2025 && len(rep.Allocation.Rows) > 0 {
		switch {
		case rep.Allocation.Confirmed == "":
			add("allocation", "warn", "Jan 1, 2025 split not confirmed",
				"Check the accounts, then confirm.", "#taxes")
		case rep.Allocation.Changed:
			add("allocation", "warn", "Jan 1, 2025 split changed since confirmed", "Confirm it again.", "#taxes")
		default:
			add("allocation", "ok", "Jan 1, 2025 split confirmed", "", "")
		}
	}

	// Coin choices.
	for _, id := range rep.Identifications {
		if id.Status == "late" && in(id.CreatedAt) {
			add("ident-"+strconv.FormatInt(id.ID, 10), "warn", "Coin choice recorded after its sale, not used", id.Note, "#overview")
		}
		if id.Status == "waiting" && time.Since(id.CreatedAt) > autoWindow && in(id.CreatedAt) {
			add("ident-"+strconv.FormatInt(id.ID, 10), "warn", "Coin choice with no sale",
				"Tie it to its sale, or cancel it.", "#overview")
		}
	}

	// Income without a type, and rewards that may be rebates.
	var other, rewards int
	for _, i := range rep.Income {
		if !in(i.Time) {
			continue
		}
		switch i.Type {
		case "other":
			other++
		case "rewards":
			rewards++
		}
	}
	if other > 0 {
		add("income-type", "warn", fmt.Sprintf("%d income record%s with no type", other, plural(other)),
			"Set a type for each.", "#taxes")
	}
	if rewards > 0 {
		add("rewards", "warn", fmt.Sprintf("%d reward%s to ask about", rewards, plural(rewards)),
			"Cashback may be a rebate, not income. Ask your tax professional.", "#taxes")
	}

	// Gifts and donations needing details.
	var giftFlags int
	for _, g := range rep.Gifts {
		if in(time.Unix(g.Time, 0)) && len(g.Flags) > 0 {
			giftFlags++
		}
	}
	if giftFlags > 0 {
		add("gifts", "warn", fmt.Sprintf("%d gift%s or donation%s to check", giftFlags, plural(giftFlags), plural(giftFlags)),
			"See Details.", "#taxes")
	}

	// Data that may not cover the whole year.
	if year < time.Now().In(z).Year() {
		accts, _ := s.st.Accounts()
		imps, _ := s.st.Imports()
		latest := map[string]string{}
		for _, im := range imps {
			if im.ImportedAt > latest[im.Account] {
				latest[im.Account] = im.ImportedAt
			}
		}
		var stale []string
		for _, a := range accts {
			last := latest[a.Name]
			if a.Kind == "wallet" {
				last = a.ScannedAt
			}
			if a.Kind != "wallet" && a.Kind != "exchange" {
				continue
			}
			if t, err := time.Parse(time.RFC3339, last); err != nil || t.Before(end) {
				stale = append(stale, a.Name)
			}
		}
		if len(stale) > 0 {
			add("fresh", "warn", "Data ends before Dec 31: "+strings.Join(stale, ", "),
				"Import a fresh export or recheck.", "#imports")
		}
	}

	// Tracked and on-chain balances that differ.
	var off []string
	for _, a := range rep.Accounts {
		if a.ChainSats != nil && *a.ChainSats != a.Sats {
			off = append(off, a.Name)
		}
	}
	if len(off) > 0 {
		add("balances", "warn", "Don't match chain: "+strings.Join(off, ", "),
			"Unlabeled, mislinked or stale. See Overview.", "#overview")
	}

	rd.Reviewed = s.st.Setting(fmt.Sprintf("taxReviewed:%d", year), "")
	rd.Downloaded = s.st.Setting(fmt.Sprintf("taxDownloaded:%d", year), "")
	rd.Ready = true
	for _, c := range rd.Checks {
		if c.Status == "bad" {
			rd.Ready = false
		}
	}
	return rd, nil
}

func (s *Server) getReadiness(w http.ResponseWriter, r *http.Request) {
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	if year == 0 {
		year = time.Now().In(loc(rep.Settings)).Year() - 1
	}
	rd, err := s.readiness(rep, year)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, rd)
}

// postReviewed marks a year's numbers as reviewed by the user, or not.
func (s *Server) postReviewed(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(r.PathValue("year"))
	var in struct{ Reviewed bool }
	if err := decode(r, &in); err != nil || year < 2009 {
		fail(w, 400, fmt.Errorf("which tax year?"))
		return
	}
	v := ""
	if in.Reviewed {
		v = time.Now().UTC().Format(time.RFC3339)
	}
	s.st.SetSetting(fmt.Sprintf("taxReviewed:%d", year), v)
	writeJSON(w, map[string]string{"reviewed": v})
}
