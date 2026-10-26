package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lightning"
)

// lightningSync pulls the node's bookkeeper history and balances over
// CLNRest. It is the only contact with the node and it only reads.
func (s *Server) lightningSync(ctx context.Context) (map[string]any, error) {
	if s.demo != nil {
		return nil, errNoNode
	}
	u := s.st.Setting("clnUrl", "")
	if u == "" {
		return nil, errors.New("set your node's CLNRest address in Settings first")
	}
	c, err := lightning.Dial(u, s.st.Setting("clnRune", ""), s.st.Setting("clnCert", ""))
	if err != nil {
		return nil, err
	}
	alias, network, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	if c.CertSHA256 != "" && s.st.Setting("clnCert", "") == "" {
		s.st.SetSetting("clnCert", c.CertSHA256) // trust on first use, like Fulcrum
	}
	evs, err := c.Income(ctx)
	if err != nil {
		return nil, err
	}
	chans, err := c.Balances(ctx)
	if err != nil {
		return nil, err
	}
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	added, err := s.st.PutLightning(evs)
	if err != nil {
		return nil, err
	}
	cj, _ := json.Marshal(chans)
	s.st.SetSetting("clnChannels", string(cj))
	s.st.SetSetting("clnAlias", alias)
	s.st.SetSetting("clnSynced", time.Now().UTC().Format(time.RFC3339))
	var bal int64
	for _, ch := range chans {
		bal += ch.Sats
	}
	return map[string]any{"alias": alias, "network": network, "events": len(evs), "added": added, "balanceSats": bal, "cert": c.CertSHA256}, nil
}

func (s *Server) syncLightning(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := s.lightningSync(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, out)
}
