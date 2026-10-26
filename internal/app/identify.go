package app

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lots"
	"github.com/DeGrandis/proof-of-basis/internal/store"
)

// Lot identification: which coins a sale or spend uses, recorded before
// it happens (IRS FAQ 82: no later than the date and time of the sale).
// A recorded choice is applied to the sale it belongs to; one recorded
// after the sale never is.

// IdentView is a recorded identification and what became of it.
type IdentView struct {
	store.Identification
	Status    string `json:"status"` // waiting, applied, late, cancelled
	AppliedTo string `json:"appliedTo,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

const autoWindow = 30 * 24 * time.Hour

// applyIdentifications sets each recorded choice's picks on the sale it
// belongs to: the record it names, or else the first sale or spend from
// that account within 30 days after it, of about the same amount.
func applyIdentifications(ops []lots.Op, ids []store.Identification, kind map[string]string) []IdentView {
	used := map[int]bool{}
	out := make([]IdentView, 0, len(ids))
	for _, id := range ids {
		v := IdentView{Identification: id, Status: "waiting"}
		if id.Cancelled {
			v.Status = "cancelled"
			out = append(out, v)
			continue
		}
		best := -1
		for i, o := range ops {
			if used[i] || len(o.Picks) > 0 {
				continue
			}
			if id.AppliesTo != "" {
				if o.ID == id.AppliesTo || strings.HasPrefix(o.ID, id.AppliesTo+">") {
					best = i
					break
				}
				continue
			}
			if o.Account != id.Account || (o.Kind != lots.Dispose && o.Kind != lots.Remove) {
				continue
			}
			if o.Time.Before(id.CreatedAt) || o.Time.After(id.CreatedAt.Add(autoWindow)) {
				continue
			}
			tol := max(id.Sats/100, 1000)
			if d := o.Sats - id.Sats; d < -tol || d > tol {
				continue
			}
			if best < 0 || o.Time.Before(ops[best].Time) {
				best = i
			}
		}
		if best >= 0 {
			o := &ops[best]
			if o.Time.Before(id.CreatedAt) {
				v.Status, v.AppliedTo = "late", o.ID
				v.Warning = "Recorded after the sale, so it can't be used: the IRS requires the choice no later than the sale."
			} else {
				used[best] = true
				for _, p := range id.Picks {
					o.Picks = append(o.Picks, lots.Pick{OriginID: p.OriginID, Sats: p.Sats})
				}
				v.Status, v.AppliedTo = "applied", o.ID
			}
		}
		if kind[id.Account] == "exchange" && id.CreatedAt.Year() >= 2026 {
			v.Warning = strings.TrimSpace(v.Warning + " For coins held at an exchange from 2026, the exchange must also have this choice (or a standing order) at the time of the sale; this record alone isn't enough.")
		}
		out = append(out, v)
	}
	return out
}

// postIdentification records a choice of coins before a sale.
func (s *Server) postIdentification(w http.ResponseWriter, r *http.Request) {
	var in store.Identification
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if in.Account == "" || len(in.Picks) == 0 {
		fail(w, 400, errors.New("give the account and the coins chosen"))
		return
	}
	p, err := s.pipeline()
	if err != nil {
		fail(w, 500, err)
		return
	}
	res, err := lots.Run(p.tr.Ops, p.cfg)
	if err != nil {
		fail(w, 500, err)
		return
	}
	// Each pick must be held in that account now; keep its date and basis.
	held := map[string]*lots.Lot{}
	for i := range res.Open {
		l := &res.Open[i]
		if l.Account != in.Account {
			continue
		}
		if h := held[l.OriginID]; h != nil {
			h.Sats += l.Sats
			h.Basis += l.Basis
		} else {
			c := *l
			held[l.OriginID] = &c
		}
	}
	in.Sats = 0
	for i := range in.Picks {
		pk := &in.Picks[i]
		h := held[pk.OriginID]
		if h == nil || h.Sats < pk.Sats || pk.Sats <= 0 {
			fail(w, 400, fmt.Errorf("%s doesn't hold those coins from %s any more; plan again", in.Account, pk.OriginID))
			return
		}
		pk.Acquired, pk.Basis = h.Acquired, lots.MulDiv(h.Basis, pk.Sats, h.Sats)
		in.Sats += pk.Sats
	}
	in.ID, in.Cancelled, in.CreatedAt = 0, false, time.Now()
	in.Note = clip(in.Note, 300)
	id, err := s.st.AddIdentification(in)
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.note("identify", fmt.Sprintf("Recorded which coins to use from %s", in.Account))
	writeJSON(w, map[string]any{"id": id, "createdAt": in.CreatedAt.UTC()})
}

// patchIdentification ties a choice to a sale, or cancels it.
func (s *Server) patchIdentification(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		AppliesTo string
		Cancelled bool
	}
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.st.UpdateIdentification(id, strings.TrimSpace(in.AppliesTo), in.Cancelled); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
