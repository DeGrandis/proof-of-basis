package importers

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// DARow is one Form 1099-DA transaction, as a broker reports it. Only the
// boxes that matter for reconciling a Bitcoin sale are kept.
type DARow struct {
	Row           int       `json:"row"`
	Asset         string    `json:"asset"`              // box 1b
	Sats          int64     `json:"sats"`               // box 1c
	Acquired      time.Time `json:"acquired,omitzero"`  // box 1d; zero when "various"
	Sold          time.Time `json:"sold"`               // box 1e
	Proceeds      int64     `json:"proceeds"`           // box 1f, net of transaction costs
	Basis         int64     `json:"basis"`              // box 1g
	HasBasis      bool      `json:"hasBasis"`           // box 1g filled in
	BasisReported bool      `json:"basisReported"`      // box 2
	Term          string    `json:"term,omitempty"`     // box 6: "short", "long" or ""
	Noncovered    bool      `json:"noncovered"`         // box 9
	Withheld      int64     `json:"withheld,omitempty"` // box 4
}

// Columns by box number or by the form's own wording; brokers' CSVs use
// either. Matching ignores case and spacing.
var daCols = struct{ asset, units, acquired, sold, proceeds, basis, reported, term, noncovered, withheld []string }{
	asset:      []string{"1b", "Box 1b", "Name of Digital Asset", "Digital Asset", "Asset", "Asset Name", "Description"},
	units:      []string{"1c", "Box 1c", "Number of Units", "Units", "Quantity", "Amount"},
	acquired:   []string{"1d", "Box 1d", "Date Acquired", "Acquired"},
	sold:       []string{"1e", "Box 1e", "Date Sold or Disposed", "Date Sold", "Date Disposed", "Sold"},
	proceeds:   []string{"1f", "Box 1f", "Proceeds", "Gross Proceeds"},
	basis:      []string{"1g", "Box 1g", "Cost or Other Basis", "Cost Basis", "Basis"},
	reported:   []string{"2", "Box 2", "Check if Basis Reported to IRS", "Basis Reported to IRS", "Basis Reported"},
	term:       []string{"6", "Box 6", "Gain or Loss", "Term", "Short-term or Long-term", "Holding Period"},
	noncovered: []string{"9", "Box 9", "Noncovered Security", "Noncovered"},
	withheld:   []string{"4", "Box 4", "Federal Income Tax Withheld", "Withholding"},
}

// Parse1099DA reads a broker's Form 1099-DA export (CSV) and keeps the
// Bitcoin rows.
func Parse1099DA(r io.Reader) ([]DARow, []Warning, error) {
	t, err := ReadTable(r)
	if err != nil {
		return nil, nil, err
	}
	c := daCols
	ix := func(a []string) int { return t.Col(a...) }
	iUnits, iSold, iProceeds := ix(c.units), ix(c.sold), ix(c.proceeds)
	if iUnits < 0 || iSold < 0 || iProceeds < 0 {
		return nil, nil, fmt.Errorf("1099-DA: need the number of units (1c), date sold (1e) and proceeds (1f) columns")
	}
	iAsset, iAcq, iBasis, iRep, iTerm, iNon, iWith := ix(c.asset), ix(c.acquired), ix(c.basis), ix(c.reported), ix(c.term), ix(c.noncovered), ix(c.withheld)
	var out []DARow
	var warns []Warning
	for n, row := range t.Rows {
		line := t.FirstRow + n
		w := func(f string, a ...any) { warns = append(warns, Warning{line, fmt.Sprintf(f, a...)}) }
		asset := cell(row, iAsset)
		if a := strings.ToLower(asset); a != "" && !strings.Contains(a, "bitcoin") && a != "btc" && !strings.HasPrefix(a, "btc ") {
			continue
		}
		sold, err := ParseTime(cell(row, iSold))
		if err != nil {
			w("date sold: %v", err)
			continue
		}
		units, err1 := Sats(cell(row, iUnits))
		proceeds, err2 := Cents(cell(row, iProceeds))
		if err := firstErr(err1, err2); err != nil {
			w("%v", err)
			continue
		}
		d := DARow{Row: line, Asset: asset, Sats: abs(units), Sold: sold, Proceeds: proceeds,
			BasisReported: checked(cell(row, iRep)), Noncovered: checked(cell(row, iNon))}
		if a := cell(row, iAcq); a != "" && !strings.EqualFold(a, "various") {
			if d.Acquired, err = ParseTime(a); err != nil {
				w("date acquired: %v", err)
			}
		}
		if b := cell(row, iBasis); b != "" {
			if d.Basis, err = Cents(b); err != nil {
				w("basis: %v", err)
			} else {
				d.HasBasis = true
			}
		}
		if v := cell(row, iWith); v != "" {
			d.Withheld, _ = Cents(v)
		}
		switch t := strings.ToLower(cell(row, iTerm)); {
		case strings.Contains(t, "short"):
			d.Term = "short"
		case strings.Contains(t, "long"):
			d.Term = "long"
		}
		out = append(out, d)
	}
	return out, warns, nil
}

// checked reads a checkbox column: x, yes, true, 1, checked.
func checked(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "x", "yes", "y", "true", "1", "checked", "reported":
		return true
	}
	return false
}
