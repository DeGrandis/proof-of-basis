package app

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"btctax/internal/importers"
	"btctax/internal/store"
)

// Exchanges send Form 1099-DA as a PDF. The app keeps it with the year's
// records and puts it in the accountant package, and checks it by its
// totals: the user types the form's total proceeds (and basis, if it
// shows one) and the app compares them with its own sales for that
// exchange and year. A line-by-line check needs the lines themselves
// (a CSV, where an exchange offers one).

// FormCheck is one exchange's 1099-DA for a year against the records.
type FormCheck struct {
	Account      string               `json:"account"`
	Forms        []store.TaxForm      `json:"forms"`
	Lines        bool                 `json:"lines"` // 1099-DA lines imported: checked line by line
	Sales        int                  `json:"sales"`
	Proceeds     int64                `json:"proceeds"`
	Basis        int64                `json:"basis"`
	ByBox        map[string]*[2]int64 `json:"byBox"` // box -> proceeds, basis
	FormProceeds *int64               `json:"formProceeds,omitempty"`
	FormBasis    *int64               `json:"formBasis,omitempty"`
	Status       string               `json:"status"` // lines, no form, no totals, match, differs
	Diff         int64                `json:"diff,omitempty"`
}

// formChecks lists, for a year from 2025, each exchange with sales or a
// form, and how its 1099-DA compares.
func (s *Server) formChecks(f *Forms, year int) ([]FormCheck, error) {
	if year < 2025 {
		return []FormCheck{}, nil
	}
	accts, err := s.st.Accounts()
	if err != nil {
		return nil, err
	}
	exchange := map[string]bool{}
	for _, a := range accts {
		exchange[a.Name] = a.Kind == "exchange"
	}
	pdfs, err := s.st.TaxForms(year)
	if err != nil {
		return nil, err
	}
	by := map[string]*FormCheck{}
	get := func(acct string) *FormCheck {
		if by[acct] == nil {
			by[acct] = &FormCheck{Account: acct, Forms: []store.TaxForm{}, ByBox: map[string]*[2]int64{}}
		}
		return by[acct]
	}
	for _, r := range f.Rows {
		if !exchange[r.Account] {
			continue
		}
		c := get(r.Account)
		c.Sales++
		c.Proceeds += r.OurProceeds
		c.Basis += r.OurBasis
		if c.ByBox[r.Box] == nil {
			c.ByBox[r.Box] = &[2]int64{}
		}
		c.ByBox[r.Box][0] += r.OurProceeds
		c.ByBox[r.Box][1] += r.OurBasis
	}
	for _, x := range f.Forms {
		get(x.Account).Lines = true
	}
	for _, p := range pdfs {
		c := get(p.Account)
		c.Forms = append(c.Forms, p)
		if t := p.Totals.Proceeds; t != nil {
			c.FormProceeds = addPtr(c.FormProceeds, *t)
		}
		if t := p.Totals.Basis; t != nil {
			c.FormBasis = addPtr(c.FormBasis, *t)
		}
	}
	out := make([]FormCheck, 0, len(by))
	for _, c := range by {
		switch {
		case c.Lines:
			c.Status = "lines"
		case len(c.Forms) == 0:
			c.Status = "no form"
		case c.FormProceeds == nil:
			c.Status = "no totals"
		default:
			c.Diff = *c.FormProceeds - c.Proceeds
			// Forms round per line; allow a dollar and a cent a sale.
			if tol := int64(100 + c.Sales); c.Diff >= -tol && c.Diff <= tol {
				c.Status = "match"
			} else {
				c.Status = "differs"
			}
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

// moreLess says how the form's total compares: "$5.00 more" or "less".
func moreLess(diff int64) string {
	if diff < 0 {
		return usdText(-diff) + " less"
	}
	return usdText(diff) + " more"
}

func addPtr(p *int64, v int64) *int64 {
	if p == nil {
		return &v
	}
	n := *p + v
	return &n
}

func parseCents(s string) (*int64, error) {
	if s = strings.TrimSpace(s); s == "" {
		return nil, nil
	}
	c, err := importers.Cents(s)
	if err != nil {
		return nil, fmt.Errorf("%q isn't an amount", s)
	}
	return &c, nil
}

func (s *Server) listTaxForms(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	list, err := s.st.TaxForms(year)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, list)
}

// postTaxForm keeps a 1099-DA as the exchange sent it, with its totals.
func (s *Server) postTaxForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachment+1<<20)
	if err := r.ParseMultipartForm(maxAttachment + 1<<20); err != nil {
		fail(w, 400, errors.New("files can be up to 10 MB"))
		return
	}
	acct := strings.TrimSpace(r.FormValue("account"))
	year, _ := strconv.Atoi(r.FormValue("year"))
	if acct == "" || year < 2025 {
		fail(w, 400, errors.New("pick the exchange and a tax year from 2025"))
		return
	}
	proceeds, err1 := parseCents(r.FormValue("proceeds"))
	basis, err2 := parseCents(r.FormValue("basis"))
	if err := firstError(err1, err2); err != nil {
		fail(w, 400, err)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, errors.New("choose the 1099-DA file"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAttachment+1))
	if err != nil || len(data) > maxAttachment {
		fail(w, 400, errors.New("files can be up to 10 MB"))
		return
	}
	typ := http.DetectContentType(data)
	if t, _, err := mime.ParseMediaType(typ); err == nil {
		typ = t
	}
	f, err := s.st.AddTaxForm(store.TaxForm{Account: acct, Year: year, Filename: filepath.Base(strings.ReplaceAll(hdr.Filename, "\\", "/")), Mime: typ,
		Totals: store.FormTotal{Proceeds: proceeds, Basis: basis}}, data)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, f)
}

// postTaxFormTotals sets (or clears, with "") the totals typed from a form.
func (s *Server) postTaxFormTotals(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Proceeds, Basis string }
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	p, err1 := parseCents(in.Proceeds)
	b, err2 := parseCents(in.Basis)
	if err := firstError(err1, err2); err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.st.SetTaxFormTotals(id, store.FormTotal{Proceeds: p, Basis: b}); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) getTaxForm(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f, data, err := s.st.TaxFormData(id)
	if err != nil {
		fail(w, 404, errors.New("no such form"))
		return
	}
	typ := "application/octet-stream"
	if safeTypes[f.Mime] {
		typ = f.Mime
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Filename))
	w.Write(data)
}

func (s *Server) deleteTaxForm(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteTaxForm(id); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func firstError(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
