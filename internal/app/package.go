package app

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"btctax/internal/lots"
)

// The accountant package: one download per tax year with everything a tax
// professional needs to prepare the return and check the numbers, and the
// proof behind them. It documents; it does not file anything.

func (s *Server) getPackage(w http.ResponseWriter, r *http.Request) {
	year, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("year"), ".zip"))
	if err != nil || year < 2009 {
		fail(w, 400, fmt.Errorf("which tax year?"))
		return
	}
	var buf bytes.Buffer
	if err := s.writePackage(&buf, year, s.exportIDs(r.URL.Query().Get("onchain") == "1")); err != nil {
		fail(w, 500, err)
		return
	}
	s.note("export", fmt.Sprintf("Downloaded the accountant package for %d", year))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="btc-tax-package-%d.zip"`, year))
	w.Write(buf.Bytes())
}

func (s *Server) writePackage(out io.Writer, year int, ids exportIDs) error {
	rep, err := s.buildReport()
	if err != nil {
		return err
	}
	p, err := s.pipeline()
	if err != nil {
		return err
	}
	f, err := s.forms(rep, year)
	if err != nil {
		return err
	}
	rd, err := s.readiness(rep, year)
	if err != nil {
		return err
	}
	z := loc(rep.Settings)
	start := time.Date(year, 1, 1, 0, 0, 0, 0, z)
	end := start.AddDate(1, 0, 0)
	in := func(t time.Time) bool { return !t.Before(start) && t.Before(end) }

	zw := zip.NewWriter(out)
	dir := fmt.Sprintf("btc-tax-%d/", year)
	file := func(name string) (io.Writer, error) {
		return zw.CreateHeader(&zip.FileHeader{Name: dir + name, Method: zip.Deflate, Modified: time.Now()})
	}
	table := func(name string, header []string, rows [][]string) error {
		w, err := file(name)
		if err != nil {
			return err
		}
		cw := csv.NewWriter(w)
		cw.Write(header)
		cw.WriteAll(rows)
		cw.Flush()
		return cw.Error()
	}
	var files []string
	note := func(name, what string) { files = append(files, fmt.Sprintf("  %-34s %s", name, what)) }

	// Form 8949: all lines, and one file per box.
	w, err := file(fmt.Sprintf("form8949-%d.csv", year))
	if err != nil {
		return err
	}
	write8949(w, f.Rows, ids)
	note(fmt.Sprintf("form8949-%d.csv", year), "every Form 8949 line, with its box, code and adjustment")
	boxes := make([]string, 0, len(f.Boxes))
	for b := range f.Boxes {
		boxes = append(boxes, b)
	}
	sort.Strings(boxes)
	for _, b := range boxes {
		var rows []F8949Row
		for _, x := range f.Rows {
			if x.Box == b {
				rows = append(rows, x)
			}
		}
		name := fmt.Sprintf("form8949-%d-box-%s.csv", year, b)
		if w, err = file(name); err != nil {
			return err
		}
		write8949(w, rows, ids)
		note(name, fmt.Sprintf("the %d line%s for box %s", len(rows), plural(len(rows)), b))
	}
	if year >= 2025 {
		name := fmt.Sprintf("1099da-reconciliation-%d.csv", year)
		if w, err = file(name); err != nil {
			return err
		}
		writeReconciliation(w, f, ids)
		note(name, "each sale against the brokers' Forms 1099-DA")
	}

	// Provenance: each lot sold, from its purchase through every transfer.
	ops := map[string]lots.Op{}
	for _, o := range p.tr.Ops {
		ops[o.ID] = o
	}
	refs := map[string]string{}
	for _, e := range p.ex {
		refs["ex:"+e.ID] = firstNonEmpty(ids.platform(e.Ref), ids.tx(e.Txid))
	}
	describe := func(id string) string {
		if acct, ok := strings.CutPrefix(id, lots.PoolSplit); ok {
			return "pooled across your accounts until 2025, then given to " + acct + " in the January 1, 2025 split"
		}
		o, ok := ops[id]
		if !ok {
			return id
		}
		s := fmt.Sprintf("%s %s", o.Time.In(z).Format("2006-01-02"), o.Account)
		if o.To != "" && o.To != o.Account {
			s += " to " + o.To
		}
		if t := ids.tx(txidOf(id)); t != "" {
			s += " (tx " + t + ")"
		} else if r := refs[strings.Split(id, ">")[0]]; r != "" {
			s += " (ID " + r + ")"
		}
		return s
	}
	var prov [][]string
	for _, d := range rep.Disposals {
		if !in(d.Disposed) {
			continue
		}
		term := "short"
		if d.LongTerm {
			term = "long"
		}
		bought, from := "unknown: no purchase found", ""
		if !d.Missing {
			bought = d.Acquired.In(z).Format("2006-01-02")
			from = describe(d.OriginID)
		}
		hops := make([]string, len(d.Path))
		for i, h := range d.Path {
			hops[i] = describe(h)
		}
		prov = append(prov, []string{d.Disposed.In(z).Format("2006-01-02"), ids.rec(strings.Split(d.OpID, ">")[0]), d.Account, satsStr(d.Sats), money(d.Proceeds), money(d.Basis),
			money(d.Gain()), term, bought, from, strings.Join(hops, " | "), ids.rec(d.OriginID)})
	}
	if err := table(fmt.Sprintf("lots-sold-%d.csv", year), []string{"Sold", "Sale reference", "Account", "BTC", "Proceeds", "Basis", "Gain", "Term",
		"Bought", "Purchase", "Moved through (oldest first)", "Purchase reference"}, prov); err != nil {
		return err
	}
	note(fmt.Sprintf("lots-sold-%d.csv", year), "each lot sold: where it was bought and every transfer since")

	// Income.
	var inc [][]string
	src := map[string]string{}
	for _, g := range rep.Graph {
		src[g.Key] = g.ValueSource
	}
	for _, i := range rep.Income {
		if !in(i.Time) {
			continue
		}
		inc = append(inc, []string{i.Time.In(z).Format("2006-01-02 15:04"), i.Account, satsStr(i.Sats), money(i.Cents), src[i.Key], i.Type, rep.IncomeTypes[i.Type],
			ids.rec(i.Key), firstNonEmpty(ids.tx(txidOf(i.Key)), refs[i.Key])})
	}
	if err := table(fmt.Sprintf("income-%d.csv", year), []string{"Received", "Account", "BTC", "Value (USD)", "Value from", "Type", "Usually reported on", "Reference", "Platform ID"}, inc); err != nil {
		return err
	}
	note(fmt.Sprintf("income-%d.csv", year), "income received, its value and where that value came from")

	// Gifts, donations, losses, gifts received.
	var gifts [][]string
	for _, g := range rep.Gifts {
		t := time.Unix(g.Time, 0)
		if !in(t) {
			continue
		}
		ded := ""
		if g.Deduction > 0 {
			ded = money(g.Deduction)
		}
		gifts = append(gifts, []string{t.In(z).Format("2006-01-02"), g.Category, g.Recipient, g.Account, satsStr(g.Sats), money(g.Value), money(g.Basis), g.Acquired, g.Term, ded,
			strings.Join(g.Flags, " "), ids.rec(g.Key)})
	}
	if len(gifts) > 0 {
		if err := table(fmt.Sprintf("gifts-donations-%d.csv", year), []string{"Date", "What", "To or from", "Account", "BTC", "Value", "Basis", "Bought", "Held", "Usual deduction", "To check", "Reference"}, gifts); err != nil {
			return err
		}
		note(fmt.Sprintf("gifts-donations-%d.csv", year), "gifts, donations and losses, with what to check")
	}

	// Transfers between the user's own accounts, and how each was matched.
	var moves [][]string
	for _, l := range rep.Links {
		o, ok := ops[l.From+">"+l.To]
		if !ok || !in(o.Time) {
			continue
		}
		moves = append(moves, []string{o.Time.In(z).Format("2006-01-02"), o.Account, o.To, satsStr(o.Sats), satsStr(l.FeeSats), l.Method,
			firstNonEmpty(ids.tx(txidOf(l.To)), ids.tx(txidOf(l.From)), refs[l.From], refs[l.To]), ids.rec(l.From), ids.rec(l.To)})
	}
	if err := table(fmt.Sprintf("transfers-%d.csv", year), []string{"Date", "From", "To", "BTC arrived", "Network fee (BTC)", "Matched by", "Platform ID", "Outflow reference", "Inflow reference"}, moves); err != nil {
		return err
	}
	note(fmt.Sprintf("transfers-%d.csv", year), "moves between your own accounts (not taxable) and how each was matched")

	// The January 2025 split, with its confirmation.
	if len(rep.Allocation.Rows) > 0 {
		var al [][]string
		for _, a := range rep.Allocation.Rows {
			how := "default"
			if a.Assigned {
				how = "assigned by you"
			}
			al = append(al, []string{time.Unix(a.Acquired, 0).In(z).Format("2006-01-02"), a.BoughtOn, a.Account, satsStr(a.Sats), money(a.Basis), how, ids.rec(a.OriginID)})
		}
		if err := table("allocation-2025-01-01.csv", []string{"Bought", "Bought on", "Belongs to on Jan 1 2025", "BTC", "Basis", "How", "Purchase reference"}, al); err != nil {
			return err
		}
		note("allocation-2025-01-01.csv", "how pre-2025 basis was split across accounts (Rev. Proc. 2024-28)")
	}

	// Coin choices recorded before sales.
	var choices [][]string
	for _, id := range rep.Identifications {
		applied := id.AppliedTo
		if !in(id.CreatedAt) && (applied == "" || !in(ops[applied].Time)) {
			continue
		}
		var picks []string
		for _, pk := range id.Picks {
			picks = append(picks, fmt.Sprintf("%s BTC bought %s (%s)", satsStr(pk.Sats), pk.Acquired.In(z).Format("2006-01-02"), ids.rec(pk.OriginID)))
		}
		if applied != "" {
			applied = ids.rec(strings.Split(applied, ">")[0])
		}
		choices = append(choices, []string{id.CreatedAt.UTC().Format(time.RFC3339), id.Account, satsStr(id.Sats), id.Strategy, strings.Join(picks, "; "), id.Status, applied, id.Note, id.Warning})
	}
	if len(choices) > 0 {
		if err := table(fmt.Sprintf("coin-choices-%d.csv", year), []string{"Recorded (UTC)", "Account", "BTC", "How chosen", "Coins chosen", "Status", "Used for", "Note", "Warning"}, choices); err != nil {
			return err
		}
		note(fmt.Sprintf("coin-choices-%d.csv", year), "which coins were chosen, recorded before each sale (IRS FAQ 82)")
	}

	// The exchanges' 1099-DAs as they sent them.
	pdfs, _ := s.st.TaxForms(year)
	for _, tf := range pdfs {
		_, data, err := s.st.TaxFormData(tf.ID)
		if err != nil {
			continue
		}
		name := fmt.Sprintf("1099-DA/%s/%s", safeName(tf.Account), safeName(tf.Filename))
		w, err := file(name)
		if err != nil {
			return err
		}
		w.Write(data)
	}
	if len(pdfs) > 0 {
		note("1099-DA/", fmt.Sprintf("the %d Form%s 1099-DA the exchanges sent", len(pdfs), plural(len(pdfs))))
	}

	// Where the records came from.
	imps, _ := s.st.Imports()
	accts, _ := s.st.Accounts()
	das, _ := s.st.Forms1099DA()
	var srcs [][]string
	for _, im := range imps {
		srcs = append(srcs, []string{"exchange export", im.Account, im.Handler, im.Filename, im.ImportedAt, strconv.Itoa(im.Rows), im.SHA256})
	}
	for _, a := range accts {
		if a.Kind == "wallet" {
			srcs = append(srcs, []string{"wallet scan", a.Name, a.Script, "", a.ScannedAt, "", ""})
		}
	}
	for _, d := range das {
		srcs = append(srcs, []string{"Form 1099-DA", d.Account, "", d.Filename, d.ImportedAt, strconv.Itoa(len(d.Rows)), ""})
	}
	if err := table("sources.csv", []string{"Kind", "Account", "Platform or script", "File", "Imported or scanned", "Rows", "SHA-256 of the file"}, srcs); err != nil {
		return err
	}
	note("sources.csv", "every file and wallet scan the records come from, with fingerprints")

	// Proof attached to this year's records.
	recTime := map[string]time.Time{}
	for _, g := range rep.Graph {
		recTime[g.Key] = time.Unix(g.Time, 0)
	}
	atts, _ := s.st.Attachments("")
	var attached int
	for _, a := range atts {
		t, ok := recTime[a.Key]
		if !ok || !in(t) {
			continue
		}
		_, data, err := s.st.AttachmentData(a.ID)
		if err != nil {
			continue
		}
		name := fmt.Sprintf("proof/%s/%d-%s", safeName(ids.rec(a.Key)), a.ID, safeName(a.Filename))
		w, err := file(name)
		if err != nil {
			return err
		}
		w.Write(data)
		attached++
	}
	if attached > 0 {
		note("proof/", fmt.Sprintf("%d file%s you attached to records as proof", attached, plural(attached)))
	}

	// The README a tax professional reads first.
	w, err = file("README.txt")
	if err != nil {
		return err
	}
	io.WriteString(w, packageReadme(rep, rd, f, year, files, s, ids.onchain))
	return zw.Close()
}

var nonName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = nonName.ReplaceAllString(path.Base(strings.ReplaceAll(s, "\\", "/")), "_")
	if s == "" || s == "." || s == ".." {
		return "file"
	}
	return s
}

// txidOf finds the on-chain transaction id in a record key.
func txidOf(key string) string {
	for _, part := range strings.Split(key, ">") {
		if strings.HasPrefix(part, "tx:") {
			if f := strings.Split(part, ":"); len(f) > 1 {
				return f[1]
			}
		}
	}
	return ""
}

func packageReadme(rep *Report, rd *Readiness, f *Forms, year int, files []string, s *Server, onchain bool) string {
	var b strings.Builder
	set := rep.Settings
	fmt.Fprintf(&b, "Bitcoin records for tax year %d\n", year)
	fmt.Fprintf(&b, "Prepared with btctax on %s. This package documents the records; it is not a tax return.\n\n", time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	var y YearSummary
	for _, x := range rep.Years {
		if x.Year == year {
			y = x
		}
	}
	b.WriteString("SUMMARY\n")
	fmt.Fprintf(&b, "  Short-term: proceeds %s, basis %s, gain %s\n", usdText(y.ShortProceeds), usdText(y.ShortBasis), usdText(y.ShortProceeds-y.ShortBasis))
	fmt.Fprintf(&b, "  Long-term:  proceeds %s, basis %s, gain %s\n", usdText(y.LongProceeds), usdText(y.LongBasis), usdText(y.LongProceeds-y.LongBasis))
	if y.CarryInShort+y.CarryInLong > 0 {
		fmt.Fprintf(&b, "  Capital loss carried in: %s short-term, %s long-term\n", usdText(y.CarryInShort), usdText(y.CarryInLong))
	}
	if y.LossDeducted > 0 {
		fmt.Fprintf(&b, "  Net capital loss deducted from other income: %s\n", usdText(y.LossDeducted))
	}
	if y.CarryOutShort+y.CarryOutLong > 0 {
		fmt.Fprintf(&b, "  Capital loss carried to %d: %s short-term, %s long-term\n", year+1, usdText(y.CarryOutShort), usdText(y.CarryOutLong))
	}
	fmt.Fprintf(&b, "  Income received in BTC: %s\n", usdText(y.Income))
	types := make([]string, 0, len(y.IncomeByType))
	for t := range y.IncomeByType {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		fmt.Fprintf(&b, "    %-16s %s (usually: %s)\n", t, usdText(y.IncomeByType[t]), rep.IncomeTypes[t])
	}
	if y.MissingBasis > 0 {
		fmt.Fprintf(&b, "  %s BTC sold with no known purchase: its basis is counted as $0\n", satsStr(y.MissingBasis))
	}
	fmt.Fprintf(&b, "\nFORM 1040 DIGITAL ASSET QUESTION\n  %s", y.DigitalAsset.Answer)
	if len(y.DigitalAsset.Reasons) > 0 {
		fmt.Fprintf(&b, ": %s", strings.Join(y.DigitalAsset.Reasons, "; "))
	}
	b.WriteString("\n")
	for _, a := range y.DigitalAsset.Ask {
		fmt.Fprintf(&b, "  To ask: %s\n", a)
	}

	if len(f.Checks) > 0 {
		b.WriteString("\nFORMS 1099-DA\n")
		for _, c := range f.Checks {
			fmt.Fprintf(&b, "  %s\n", c.Account)
			if len(c.Forms) == 0 {
				b.WriteString("    No 1099-DA from this exchange is included.\n")
			}
			for _, tf := range c.Forms {
				fmt.Fprintf(&b, "    Form: 1099-DA/%s/%s\n", safeName(tf.Account), safeName(tf.Filename))
			}
			boxes := make([]string, 0, len(c.ByBox))
			for bx := range c.ByBox {
				boxes = append(boxes, bx)
			}
			sort.Strings(boxes)
			parts := make([]string, 0, len(boxes))
			for _, bx := range boxes {
				parts = append(parts, fmt.Sprintf("box %s %s", bx, usdText(c.ByBox[bx][0])))
			}
			fmt.Fprintf(&b, "    These records: %d sale%s, proceeds %s (%s), basis %s\n", c.Sales, plural(c.Sales), usdText(c.Proceeds), strings.Join(parts, ", "), usdText(c.Basis))
			switch c.Status {
			case "lines":
				b.WriteString("    Checked line by line against the 1099-DA lines imported; see the reconciliation file.\n")
			case "match":
				fmt.Fprintf(&b, "    The form's total proceeds, %s, match these records. Checked by totals, not line by line.\n", usdText(*c.FormProceeds))
			case "differs":
				fmt.Fprintf(&b, "    The form's total proceeds are %s: %s than these records. Not yet explained.\n", usdText(*c.FormProceeds), moreLess(c.Diff))
			case "no totals":
				b.WriteString("    The form's totals weren't entered, so it hasn't been checked against these records.\n")
			case "no form":
				b.WriteString("    Not checked: no form yet.\n")
			}
			if c.FormBasis != nil {
				fmt.Fprintf(&b, "    The form reports total basis of %s; these records show %s.\n", usdText(*c.FormBasis), usdText(c.Basis))
			}
		}
		b.WriteString("  Sales without imported 1099-DA lines are placed in box H or K (reported without basis), which is right\n")
		b.WriteString("  for most 2025 forms. Please confirm box 2 (\"Check if basis reported to IRS\") on each form: where it is\n")
		b.WriteString("  checked, those sales belong in box G or J instead, with the basis the form shows.\n")
	}

	b.WriteString("\nHOW THE NUMBERS WERE WORKED OUT\n")
	method := set["method"]
	if m := s.st.Setting("method:"+strconv.Itoa(year), ""); m != "" {
		method = m
	}
	fmt.Fprintf(&b, "  Lot method: %s, except where coins were chosen before a sale (coin-choices file).\n", strings.ToUpper(method))
	if year >= 2025 {
		b.WriteString("  Basis is tracked per wallet and account (from January 1, 2025, Rev. Proc. 2024-28).\n")
		if rep.Allocation.Confirmed != "" {
			fmt.Fprintf(&b, "  The pre-2025 pool was split as in allocation-2025-01-01.csv, confirmed %s%s.\n", rep.Allocation.Confirmed,
				map[bool]string{true: " (changed since: see the file)", false: ""}[rep.Allocation.Changed])
		} else if len(rep.Allocation.Rows) > 0 {
			b.WriteString("  The pre-2025 pool split (allocation-2025-01-01.csv) has not been confirmed.\n")
		}
	} else {
		b.WriteString("  Basis is pooled across all accounts (before 2025).\n")
	}
	fee := map[string]string{"dispose": "treated as small disposals (IRS FAQs 81 and 97)", "carry": "added to the moved coins' basis (no gain)"}[set["feeMode"]]
	fmt.Fprintf(&b, "  Network fees paid in BTC on transfers between own accounts: %s.\n", fee)
	fmt.Fprintf(&b, "  Dates and holding periods use the %s time zone. Holding period: more than one year is long-term.\n", set["timezone"])
	b.WriteString("  Values: an amount entered by the user, the exchange's own figure, or a day's BTC close; income-" + strconv.Itoa(year) + ".csv names which for each.\n")
	fmt.Fprintf(&b, "  Rules: %s.\n", map[bool]string{true: "United States", false: set["jurisdiction"]}[set["jurisdiction"] == "us" || set["jurisdiction"] == ""])

	b.WriteString("\nOPEN ITEMS\n")
	open := 0
	for _, c := range rd.Checks {
		if c.Status == "ok" {
			continue
		}
		open++
		mark := "!"
		if c.Status == "bad" {
			mark = "X"
		}
		fmt.Fprintf(&b, "  [%s] %s\n", mark, c.Title)
		if c.Detail != "" {
			fmt.Fprintf(&b, "      %s\n", c.Detail)
		}
	}
	if open == 0 {
		b.WriteString("  None: every check passed.\n")
	}

	b.WriteString("\nFILES\n")
	b.WriteString(strings.Join(files, "\n"))
	b.WriteString("\n\nNOTES\n")
	b.WriteString("  Form 8949 boxes: C/F before 2025; from 2025, G/J when a 1099-DA reported basis, H/K when it didn't, I/L for sales no 1099-DA covers.\n")
	b.WriteString("  Column (g) adjustments keep the gain the records support when a 1099-DA differs (code B: basis, E: transaction costs, O: other).\n")
	if onchain {
		b.WriteString("  Record ids (ex:..., tx:<txid>:<output>) tie every line back to an exchange row or an on-chain transaction.\n")
	} else {
		b.WriteString("  References (R-...) tie every line to a record kept in the preparer's own app, where the full detail, including\n")
		b.WriteString("  on-chain transactions, is available on request. Platform IDs are the exchanges' own transaction ids.\n")
	}
	return b.String()
}
