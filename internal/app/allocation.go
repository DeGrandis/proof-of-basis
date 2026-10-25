package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"btctax/internal/lots"
)

// From 2025 basis is tracked per wallet and account. Rev. Proc. 2024-28
// lets the leftover pool be allocated to them as of January 1, 2025, and
// expects a record of how. The engine allocates by default; the user can
// assign purchases to accounts and confirm the result, which is kept.

// AllocRow is one purchase's coins given to one account at the switch.
type AllocRow struct {
	OriginID string `json:"originId"`
	BoughtOn string `json:"boughtOn"` // the account the purchase was made in
	Acquired int64  `json:"acquired"`
	Account  string `json:"account"`
	Sats     int64  `json:"sats"`
	Basis    int64  `json:"basis"`
	Assigned bool   `json:"assigned"` // by the user rather than the default
}

type AllocationView struct {
	Rows      []AllocRow        `json:"rows"`
	Prefs     map[string]string `json:"prefs"`
	Confirmed string            `json:"confirmed,omitempty"`
	Changed   bool              `json:"changed"` // differs from what was confirmed
}

type allocSnapshot struct {
	At   string     `json:"at"`
	Rows []AllocRow `json:"rows"`
}

func (s *Server) allocationPrefs() map[string]string {
	m := map[string]string{}
	json.Unmarshal([]byte(s.st.Setting("allocation2025", "{}")), &m)
	return m
}

// allocationView groups the engine's allocations by purchase and account.
func (s *Server) allocationView(res *lots.Result, ops []lots.Op) AllocationView {
	prefs := s.allocationPrefs()
	boughtOn := map[string]string{}
	for _, o := range ops {
		if o.Kind == lots.Acquire {
			boughtOn[o.ID] = o.Account
		}
	}
	by := map[[2]string]*AllocRow{}
	var order [][2]string
	for _, a := range res.Allocations {
		k := [2]string{a.OriginID, a.Account}
		if by[k] == nil {
			by[k] = &AllocRow{OriginID: a.OriginID, BoughtOn: boughtOn[a.OriginID], Acquired: a.Acquired.Unix(), Account: a.Account,
				Assigned: prefs[a.OriginID] == a.Account}
			order = append(order, k)
		}
		by[k].Sats += a.Sats
		by[k].Basis += a.Basis
	}
	out := make([]AllocRow, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Acquired != out[j].Acquired {
			return out[i].Acquired < out[j].Acquired
		}
		return out[i].Account < out[j].Account
	})
	v := AllocationView{Rows: out, Prefs: prefs}
	var snap allocSnapshot
	if json.Unmarshal([]byte(s.st.Setting("allocation2025Confirmed", "")), &snap) == nil && snap.At != "" {
		v.Confirmed = snap.At
		v.Changed = !sameAllocation(snap.Rows, out)
	}
	return v
}

func sameAllocation(a, b []AllocRow) bool {
	key := func(rs []AllocRow) map[[2]string]int64 {
		m := map[[2]string]int64{}
		for _, r := range rs {
			m[[2]string{r.OriginID, r.Account}] += r.Sats
		}
		return m
	}
	ka, kb := key(a), key(b)
	if len(ka) != len(kb) {
		return false
	}
	for k, v := range ka {
		if kb[k] != v {
			return false
		}
	}
	return true
}

// postAllocation assigns one pooled purchase to an account ("" restores
// the default).
func (s *Server) postAllocation(w http.ResponseWriter, r *http.Request) {
	var in struct{ OriginID, Account string }
	if err := decode(r, &in); err != nil || in.OriginID == "" {
		fail(w, 400, errors.New("give the purchase (OriginID) and the account"))
		return
	}
	prefs := s.allocationPrefs()
	if in.Account == "" {
		delete(prefs, in.OriginID)
	} else {
		prefs[in.OriginID] = in.Account
	}
	b, _ := json.Marshal(prefs)
	if _, err := s.record("user", "setting", "Changed the January 2025 per-wallet split", 0, []change{{Kind: "setting", Key: "allocation2025"}},
		func() error { return s.st.SetSetting("allocation2025", string(b)) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, prefs)
}

// confirmAllocation records the current split, with the time, as the
// allocation the user stands behind.
func (s *Server) confirmAllocation(w http.ResponseWriter, r *http.Request) {
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	if len(rep.Allocation.Rows) == 0 {
		fail(w, 400, errors.New("there is no pooled basis to split"))
		return
	}
	b, _ := json.Marshal(allocSnapshot{At: time.Now().UTC().Format(time.RFC3339), Rows: rep.Allocation.Rows})
	if _, err := s.record("user", "setting", "Confirmed the January 2025 per-wallet split", 0, []change{{Kind: "setting", Key: "allocation2025Confirmed"}},
		func() error { return s.st.SetSetting("allocation2025Confirmed", string(b)) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
