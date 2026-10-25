package app

import (
	"encoding/json"
	"errors"
	"net/http"
)

// unlinked is the set of "outflow>inflow" pairs the user said are not one
// transfer; automatic matching skips them.
func (s *Server) unlinked() map[string]bool {
	var list []string
	json.Unmarshal([]byte(s.st.Setting("unlinked", "[]")), &list)
	m := map[string]bool{}
	for _, k := range list {
		m[k] = true
	}
	return m
}

// postUnlink stops (or, with Allow, lets again) automatic matching of one
// outflow with one inflow. Both go back to Review to be labeled or linked
// by hand.
func (s *Server) postUnlink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		From, To string
		Allow    bool
	}
	if err := decode(r, &in); err != nil || in.From == "" || in.To == "" {
		fail(w, 400, errors.New("give the two records (From and To)"))
		return
	}
	m := s.unlinked()
	if in.Allow {
		delete(m, in.From+">"+in.To)
	} else {
		m[in.From+">"+in.To] = true
	}
	list := []string{}
	for k := range m {
		list = append(list, k)
	}
	b, _ := json.Marshal(list)
	summary := "Unlinked a matched transfer"
	if in.Allow {
		summary = "Allowed a transfer to be matched again"
	}
	if _, err := s.record("user", "setting", summary, 0, []change{{Kind: "setting", Key: "unlinked"}},
		func() error { return s.st.SetSetting("unlinked", string(b)) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
