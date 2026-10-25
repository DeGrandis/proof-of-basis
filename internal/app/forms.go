package app

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"btctax/internal/importers"
	"btctax/internal/store"
)

// F8949Row is one line of Form 8949: a sale (or spend) in one account,
// split by holding period, as the tax professional would enter it. Box,
// codes and adjustment come from matching it against the broker's Form
// 1099-DA when one was imported; Gain is always what the user's own
// records support.
type F8949Row struct {
	Year        int    `json:"year"`
	Box         string `json:"box"` // C/F before 2025; G-L for digital assets from 2025
	Description string `json:"description"`
	Sats        int64  `json:"sats"`
	Acquired    string `json:"acquired"` // MM/DD/YYYY or VARIOUS
	Sold        string `json:"sold"`
	Proceeds    int64  `json:"proceeds"`   // column (d)
	Basis       int64  `json:"basis"`      // column (e)
	Code        string `json:"code"`       // column (f)
	Adjustment  int64  `json:"adjustment"` // column (g)
	Gain        int64  `json:"gain"`       // column (h)
	LongTerm    bool   `json:"longTerm"`
	Account     string `json:"account"`
	Record      string `json:"record"` // the record key behind it
	TxID        string `json:"txid,omitempty"`
	Ref         string `json:"ref,omitempty"` // the platform's own transaction id
	Status      string `json:"status"`
	Note        string `json:"note,omitempty"`
	// What the user's records say, for the reconciliation.
	OurProceeds int64 `json:"ourProceeds"`
	OurBasis    int64 `json:"ourBasis"`
	DAForm      int64 `json:"daForm,omitempty"`
	DARow       int   `json:"daRow,omitempty"`
}

// DAOnly is a 1099-DA line no record of the user's matched.
type DAOnly struct {
	Form    int64           `json:"form"`
	Account string          `json:"account"`
	Row     importers.DARow `json:"row"`
	Note    string          `json:"note"`
}

// Forms is a tax year's Form 8949 lines and what didn't reconcile.
type Forms struct {
	Year   int            `json:"year"`
	Rows   []F8949Row     `json:"rows"`
	DAOnly []DAOnly       `json:"daOnly"`
	Forms  []store.DAForm `json:"forms"`
	Boxes  map[string]int `json:"boxes"`
	Issues int            `json:"issues"`
}

const (
	statusMatched  = "matches the 1099-DA"
	statusDiffers  = "on the 1099-DA, amounts differ"
	statusNotOnDA  = "not on the 1099-DA you imported"
	statusNoDA     = "no 1099-DA imported yet"
	statusNoBroker = "not a broker account: no 1099-DA expected"
	statusPre2025  = "before 1099-DA reporting"
)

// forms builds a year's Form 8949 lines from the report's disposals.
func (s *Server) forms(rep *Report, year int) (*Forms, error) {
	accts, err := s.st.Accounts()
	if err != nil {
		return nil, err
	}
	kind := map[string]string{}
	for _, a := range accts {
		kind[a.Name] = a.Kind
	}
	all, err := s.st.Forms1099DA()
	if err != nil {
		return nil, err
	}
	ex, err := s.st.ExchangeEvents()
	if err != nil {
		return nil, err
	}
	refs := map[string][2]string{} // record key -> txid, platform ref
	for _, e := range ex {
		refs["ex:"+e.ID] = [2]string{e.Txid, e.Ref}
	}

	// One line per sale and holding period.
	type group struct {
		row        F8949Row
		first, end time.Time
		missing    int64
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range rep.Disposals {
		if d.Disposed.Year() != year {
			continue
		}
		k := fmt.Sprintf("%s|%v", d.OpID, d.LongTerm)
		g := groups[k]
		if g == nil {
			rec, _, _ := strings.Cut(d.OpID, ">")
			g = &group{row: F8949Row{Year: year, LongTerm: d.LongTerm, Account: d.Account, Record: rec, Sold: d.Disposed.Format("01/02/2006")},
				first: d.Acquired, end: d.Acquired}
			if r, ok := refs[rec]; ok {
				g.row.TxID, g.row.Ref = r[0], r[1]
			} else if strings.HasPrefix(rec, "tx:") {
				g.row.TxID = strings.Split(rec, ":")[1]
			}
			groups[k] = g
			order = append(order, k)
		}
		g.row.Sats += d.Sats
		g.row.OurProceeds += d.Proceeds
		g.row.OurBasis += d.Basis
		if d.Acquired.Before(g.first) {
			g.first = d.Acquired
		}
		if d.Acquired.After(g.end) {
			g.end = d.Acquired
		}
		if d.Missing {
			g.missing += d.Sats
		}
	}

	// The year's 1099-DA lines, by account.
	type daLine struct {
		form int64
		row  importers.DARow
		used bool
	}
	da := map[string][]*daLine{}
	f := &Forms{Year: year, Rows: []F8949Row{}, DAOnly: []DAOnly{}, Forms: []store.DAForm{}, Boxes: map[string]int{}}
	for _, form := range all {
		has := false
		for _, r := range form.Rows {
			if r.Sold.Year() == year {
				da[form.Account] = append(da[form.Account], &daLine{form: form.ID, row: r})
				has = true
			}
		}
		if has {
			form.Rows = nil // the page lists forms; the lines are in Rows and DAOnly
			f.Forms = append(f.Forms, form)
		}
	}
	dayOf := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	for _, k := range order {
		g := groups[k]
		r := &g.row
		r.Description = fmt.Sprintf("%s BTC", satsStr(r.Sats))
		r.Acquired = g.first.Format("01/02/2006")
		if dayOf(g.first) != dayOf(g.end) {
			r.Acquired = "VARIOUS"
		}
		if g.missing > 0 {
			r.Note = fmt.Sprintf("%s BTC of this has no known purchase (basis counted as $0): find the missing record", satsStr(g.missing))
		}
		r.Gain = r.OurProceeds - r.OurBasis
		r.Proceeds, r.Basis = r.OurProceeds, r.OurBasis

		// Match the 1099-DA line: same account, same units, sold the same
		// day (a day either side, for time zones), same term if it says.
		sold, _ := time.Parse("01/02/2006", r.Sold)
		var best *daLine
		for _, l := range da[r.Account] {
			if l.used || l.row.Sats != r.Sats {
				continue
			}
			if l.row.Term != "" && (l.row.Term == "long") != r.LongTerm {
				continue
			}
			dd := dayOf(l.row.Sold).Sub(dayOf(sold))
			if dd < -24*time.Hour || dd > 24*time.Hour {
				continue
			}
			if best == nil || absDur(dd) < absDur(dayOf(best.row.Sold).Sub(dayOf(sold))) {
				best = l
			}
		}
		short, long := "", ""
		switch {
		case year < 2025:
			short, long, r.Status = "C", "F", statusPre2025
		case best != nil:
			best.used = true
			r.DAForm, r.DARow = best.form, best.row.Row
			r.Proceeds = best.row.Proceeds
			if best.row.BasisReported {
				short, long = "G", "J"
				r.Basis = best.row.Basis
			} else {
				short, long = "H", "K"
			}
			r.Status = statusMatched
		case kind[r.Account] == "exchange" && len(da[r.Account]) > 0:
			short, long, r.Status = "I", "L", statusNotOnDA
		case kind[r.Account] == "exchange":
			// Brokers report 2025 sales without basis; once a 1099-DA is
			// imported this line is checked against it.
			short, long, r.Status = "H", "K", statusNoDA
		default:
			short, long, r.Status = "I", "L", statusNoBroker
		}
		r.Box = short
		if r.LongTerm {
			r.Box = long
		}
		// Column (g): whatever makes (d) - (e) + (g) equal the gain the
		// records support. Code B when the reported basis is wrong, E for
		// transaction costs the proceeds don't reflect, O for anything else.
		r.Adjustment = r.Gain - (r.Proceeds - r.Basis)
		if r.Adjustment != 0 {
			var codes []string
			if r.Basis != r.OurBasis {
				codes = append(codes, "B")
			}
			if r.Proceeds > r.OurProceeds {
				codes = append(codes, "E")
			} else if r.Proceeds < r.OurProceeds {
				codes = append(codes, "O")
				r.Note = strings.TrimSpace(r.Note + " The 1099-DA shows lower proceeds than your records: ask your tax professional which to use.")
			}
			r.Code = strings.Join(codes, "")
			r.Status = statusDiffers
		}
		if r.Status == statusDiffers || r.Status == statusNotOnDA || r.Status == statusNoDA || g.missing > 0 {
			f.Issues++
		}
		f.Boxes[r.Box]++
		f.Rows = append(f.Rows, *r)
	}
	for acct, lines := range da {
		for _, l := range lines {
			if !l.used {
				f.DAOnly = append(f.DAOnly, DAOnly{Form: l.form, Account: acct, Row: l.row,
					Note: "No sale in your records matches this line: a missing import, a sale recorded on another date or amount, or a broker error."})
				f.Issues++
			}
		}
	}
	sort.SliceStable(f.DAOnly, func(i, j int) bool { return f.DAOnly[i].Row.Sold.Before(f.DAOnly[j].Row.Sold) })
	return f, nil
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func money(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

func (s *Server) getForms(w http.ResponseWriter, r *http.Request) {
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	f, err := s.forms(rep, year)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, f)
}

// write8949 writes Form 8949 lines as CSV, in the form's column order.
// Without on-chain ids, column (a) names the exchange's own id when there
// is one, and records go by their private reference.
func write8949(w io.Writer, rows []F8949Row, ids exportIDs) {
	cw := csv.NewWriter(w)
	head := []string{"Box", "(a) Description", "(b) Date acquired", "(c) Date sold or disposed", "(d) Proceeds", "(e) Cost or other basis",
		"(f) Code", "(g) Adjustment", "(h) Gain or loss", "Term", "Account", "Platform ID", "Reference", "1099-DA check", "Note"}
	if ids.onchain {
		head = append(head[:11], append([]string{"Transaction ID"}, head[11:]...)...)
	}
	cw.Write(head)
	for _, r := range rows {
		term := "Short"
		if r.LongTerm {
			term = "Long"
		}
		desc := r.Description
		if id := firstNonEmpty(ids.platform(r.Ref), ids.tx(r.TxID)); id != "" {
			desc += " (ID " + id + ")"
		}
		row := []string{r.Box, desc, r.Acquired, r.Sold, money(r.Proceeds), money(r.Basis), r.Code, money(r.Adjustment), money(r.Gain),
			term, r.Account, ids.platform(r.Ref), ids.rec(r.Record), r.Status, r.Note}
		if ids.onchain {
			row = append(row[:11], append([]string{r.TxID}, row[11:]...)...)
		}
		cw.Write(row)
	}
	cw.Flush()
}

// export8949 writes a year's Form 8949 lines, all boxes or one (?box=H).
func (s *Server) export8949(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	f, err := s.forms(rep, year)
	if err != nil {
		fail(w, 500, err)
		return
	}
	rows, name := f.Rows, fmt.Sprintf("form8949-%d.csv", year)
	if box := strings.ToUpper(r.URL.Query().Get("box")); box != "" {
		rows, name = nil, fmt.Sprintf("form8949-%d-box-%s.csv", year, box)
		for _, x := range f.Rows {
			if x.Box == box {
				rows = append(rows, x)
			}
		}
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	write8949(w, rows, s.exportIDs(r.URL.Query().Get("onchain") == "1"))
}

// writeReconciliation lists every 1099-DA line against the records.
func writeReconciliation(w io.Writer, f *Forms, ids exportIDs) {
	cw := csv.NewWriter(w)
	cw.Write([]string{"Account", "Date sold", "BTC", "Term", "1099-DA proceeds", "Your proceeds", "1099-DA basis", "Your basis", "Box", "Code", "Adjustment", "Result", "Reference", "Note"})
	for _, r := range f.Rows {
		if r.Status == statusPre2025 || r.Status == statusNoBroker {
			continue
		}
		term, daP, daB := "Short", "", ""
		if r.LongTerm {
			term = "Long"
		}
		if r.DAForm != 0 {
			daP = money(r.Proceeds)
			if r.Box == "G" || r.Box == "J" {
				daB = money(r.Basis)
			} else {
				daB = "not reported"
			}
		}
		cw.Write([]string{r.Account, r.Sold, satsStr(r.Sats), term, daP, money(r.OurProceeds), daB, money(r.OurBasis), r.Box, r.Code, money(r.Adjustment), r.Status, ids.rec(r.Record), r.Note})
	}
	for _, d := range f.DAOnly {
		basis := "not reported"
		if d.Row.HasBasis {
			basis = money(d.Row.Basis)
		}
		cw.Write([]string{d.Account, d.Row.Sold.Format("01/02/2006"), satsStr(d.Row.Sats), d.Row.Term, money(d.Row.Proceeds), "", basis, "", "", "", "", "only on the 1099-DA", "", d.Note})
	}
	cw.Flush()
}

func (s *Server) exportReconciliation(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	f, err := s.forms(rep, year)
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="1099da-reconciliation-%d.csv"`, year))
	writeReconciliation(w, f, s.exportIDs(r.URL.Query().Get("onchain") == "1"))
}

// post1099DA stores a broker's 1099-DA export for one exchange account.
func (s *Server) post1099DA(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		fail(w, 400, err)
		return
	}
	acct := strings.TrimSpace(r.FormValue("account"))
	if acct == "" {
		fail(w, 400, errors.New("pick the exchange account this 1099-DA is for"))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, errors.New("choose a 1099-DA CSV file"))
		return
	}
	defer file.Close()
	rows, warns, err := importers.Parse1099DA(io.LimitReader(file, 16<<20))
	if err != nil {
		fail(w, 400, err)
		return
	}
	if len(rows) == 0 {
		fail(w, 400, errors.New("no Bitcoin lines found in that file"))
		return
	}
	id, err := s.st.Add1099DA(acct, hdr.Filename, rows)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"id": id, "rows": len(rows), "warnings": warns})
}

func (s *Server) list1099DA(w http.ResponseWriter, r *http.Request) {
	forms, err := s.st.Forms1099DA()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, forms)
}

func (s *Server) delete1099DA(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.Delete1099DA(id); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
