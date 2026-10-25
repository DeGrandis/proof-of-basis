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
		add("basis", "bad", fmt.Sprintf("%s BTC sold or spent has no known purchase", satsStr(missing)),
			"Its basis counts as $0, which overstates the gain. Import the missing purchases or deposits, or label where the coins came from.", "#taxes")
	} else if sold > 0 {
		add("basis", "ok", "Every coin sold or spent has a known purchase", "", "")
	}

	// Review items in the year.
	var review int
	for _, it := range rep.Unmatched {
		if in(it.Time) {
			review++
		}
	}
	if review > 0 {
		add("review", "bad", fmt.Sprintf("%d record%s from %d still need%s a label", review, plural(review), year, map[bool]string{true: "s", false: ""}[review == 1]),
			"Until labeled they count for nothing, so balances and gains are incomplete.", "#review")
	} else {
		add("review", "ok", "Nothing from this year is waiting in Review", "", "")
	}

	// Accounts that went below zero by the end of the year.
	var dips []string
	for _, d := range rep.Dips {
		if time.Unix(d.T, 0).Before(end) {
			dips = append(dips, d.Account)
		}
	}
	if len(dips) > 0 {
		add("dips", "bad", "Accounts go below zero: "+strings.Join(dips, ", "),
			"They spent coins the records never show arriving. Overview explains each one.", "#overview")
	}

	// Values with no price.
	var noPrice int
	for _, g := range rep.Graph {
		if strings.HasPrefix(g.ValueSource, "no price") && in(time.Unix(g.Time, 0)) {
			noPrice++
		}
	}
	if noPrice > 0 {
		add("prices", "bad", fmt.Sprintf("%d record%s valued at $0 for lack of a price", noPrice, plural(noPrice)),
			"Update prices in Settings, or enter the value on each record.", "#settings")
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
				add(id, "warn", "No 1099-DA from "+c.Account+" yet",
					"Add the PDF the exchange sent (Taxes, Form 8949 and 1099-DA) and type its total proceeds, so your records can be checked against what the IRS was told.", "#taxes")
			case "no totals":
				add(id, "warn", "Type the total proceeds from "+c.Account+"'s 1099-DA",
					"With the form's total, the app checks your records against it.", "#taxes")
			case "differs":
				add(id, "bad", fmt.Sprintf("%s's 1099-DA shows proceeds of %s; your records show %s", c.Account, usdText(*c.FormProceeds), usdText(c.Proceeds)),
					fmt.Sprintf("The form shows %s than your records: a missing import, a sale recorded on another date or amount, or a broker error. Your tax professional will need it explained.", moreLess(c.Diff)), "#taxes")
			case "match":
				add(id, "ok", c.Account+"'s 1099-DA total matches your records", "", "")
			}
		}
		if n := f.Issues; n > 0 {
			add("reconcile", "warn", fmt.Sprintf("%d Form 8949 line%s to look at", n, plural(n)),
				"Differences from imported 1099-DA lines, sales missing from them, lines with no sale, or coins with no known purchase.", "#taxes")
		}
	}

	// The January 2025 split.
	if year >= 2025 && len(rep.Allocation.Rows) > 0 {
		switch {
		case rep.Allocation.Confirmed == "":
			add("allocation", "warn", "The January 1, 2025 per-wallet split isn't confirmed",
				"Check which account each pre-2025 purchase belongs to, then confirm to keep a dated record.", "#taxes")
		case rep.Allocation.Changed:
			add("allocation", "warn", "The January 1, 2025 split changed since you confirmed it", "Confirm it again.", "#taxes")
		default:
			add("allocation", "ok", "The January 1, 2025 split is confirmed", "", "")
		}
	}

	// Coin choices.
	for _, id := range rep.Identifications {
		if id.Status == "late" && in(id.CreatedAt) {
			add("ident-"+strconv.FormatInt(id.ID, 10), "warn", "A coin choice was recorded after its sale and wasn't used", id.Note, "#overview")
		}
		if id.Status == "waiting" && time.Since(id.CreatedAt) > autoWindow && in(id.CreatedAt) {
			add("ident-"+strconv.FormatInt(id.ID, 10), "warn", "A recorded coin choice never found its sale",
				"Tie it to the sale it was for, or cancel it.", "#overview")
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
		add("income-type", "warn", fmt.Sprintf("%d income record%s without a type", other, plural(other)),
			"Set what kind of income each was, so it lands on the right form.", "#taxes")
	}
	if rewards > 0 {
		add("rewards", "warn", fmt.Sprintf("%d reward%s to ask about", rewards, plural(rewards)),
			"Card cashback is often a purchase rebate rather than income. Ask your tax professional which these are.", "#taxes")
	}

	// Gifts and donations needing details.
	var giftFlags int
	for _, g := range rep.Gifts {
		if in(time.Unix(g.Time, 0)) && len(g.Flags) > 0 {
			giftFlags++
		}
	}
	if giftFlags > 0 {
		add("gifts", "warn", fmt.Sprintf("%d gift%s, donation%s or loss%s with something to check", giftFlags, plural(giftFlags), plural(giftFlags), map[bool]string{true: "es", false: ""}[giftFlags != 1]),
			"See Gifts, donations and losses on Taxes.", "#taxes")
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
			add("fresh", "warn", "Last imported or scanned before the year ended: "+strings.Join(stale, ", "),
				"An export or scan from before December 31 can miss the year's last records. Import a fresh export or recheck.", "#imports")
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
		add("balances", "warn", "Tracked and on-chain balances differ: "+strings.Join(off, ", "),
			"Something is unlabeled, linked wrongly, or a scan is stale. Overview shows the numbers.", "#overview")
	}

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
