package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/ollama"
	"github.com/DeGrandis/proof-of-basis/internal/scan"
)

// Demo mode: one visitor's copy of an invented history on a public server.
// There is no login, nothing connects to a real node or price source (the
// node is simulated from the copy's own data), uploads are off, and the
// assistant runs on a hosted model with a daily allowance.

// ChatModel is an assistant backend other than Ollama.
type ChatModel interface {
	Chat(ctx context.Context, req ollama.ChatRequest, onDelta, onThink func(string)) (ollama.Message, error)
	Name() string
}

type DemoOptions struct {
	AI ChatModel // nil: no assistant
	// Ask is called once per question; an error (the allowance is used
	// up) refuses it.
	Ask func() error
	// Allowance describes the assistant's limits, for Settings.
	Allowance string
}

// SetDemo turns this server into a demo copy.
func (s *Server) SetDemo(o *DemoOptions) { s.demo = o }

var errNoNode = errors.New("the demo has no real node")

const demoUploadsOff = "Uploads are off in the demo, so nobody's real files end up on a public server. Run your own copy to import yours."

// demoRoutes answer in place of the real ones in a demo copy.
func (s *Server) demoRoutes() *http.ServeMux {
	m := http.NewServeMux()
	off := func(msg string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { fail(w, http.StatusForbidden, errors.New(msg)) }
	}
	for _, p := range []string{"POST /api/imports", "POST /api/prices", "POST /api/attachments", "POST /api/taxforms", "POST /api/1099da", "POST /api/restore"} {
		m.HandleFunc(p, off(demoUploadsOff))
	}
	m.HandleFunc("POST /api/prices/fetch", off("The demo has its prices built in."))
	m.HandleFunc("/api/backup/remote", off("Advanced backup is off in the demo."))
	m.HandleFunc("/api/backup/remote/", off("Advanced backup is off in the demo."))
	m.HandleFunc("POST /api/wipe", off("To begin again, use Start over in the demo banner."))
	m.HandleFunc("POST /api/auth/", off("The demo has no login."))
	m.HandleFunc("POST /api/ai/models", off("In the demo, the assistant runs on a hosted model."))
	m.HandleFunc("GET /api/fulcrum/status", s.demoFulcrumStatus)
	m.HandleFunc("POST /api/fulcrum/test", s.demoFulcrumTest)
	m.HandleFunc("POST /api/wallets/{id}/scan", s.demoScan)
	m.HandleFunc("POST /api/recheck", s.demoRecheck)
	m.HandleFunc("POST /api/lightning/sync", s.demoLightningSync)
	return m
}

// demoHeight is a plausible current block height: about one block every
// ten minutes since the April 2024 halving.
func demoHeight() int64 {
	return 840_000 + int64(time.Since(time.Date(2024, 4, 20, 0, 9, 0, 0, time.UTC))/(10*time.Minute))
}

func (s *Server) demoFulcrumStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, fulcrumStatus{OK: true, Height: demoHeight(), Checked: time.Now().UTC()})
}

func (s *Server) demoFulcrumTest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"height": demoHeight(), "cert": ""})
}

// demoScan "scans" a wallet: the invented ones already hold their
// history; a wallet the visitor added has none.
func (s *Server) demoScan(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.Account(id)
	if err != nil || a.Kind != "wallet" {
		fail(w, 404, errors.New("no such wallet"))
		return
	}
	ws, err := s.st.WalletTxs()
	if err != nil {
		fail(w, 500, err)
		return
	}
	var txs int
	var bal int64
	found := false
	for _, x := range ws {
		if x.Account == a.Name {
			found = true
			txs = len(x.Txs)
			for _, t := range x.Txs {
				bal += t.Net()
			}
		}
	}
	if !found {
		if err := s.st.SaveScan(id, &scan.Result{Used: map[string]bool{}}); err != nil {
			fail(w, 500, err)
			return
		}
	}
	writeJSON(w, map[string]any{"balance": bal, "txs": txs, "usedAddresses": 0})
}

func (s *Server) demoRecheck(w http.ResponseWriter, r *http.Request) {
	n := 0
	if accts, err := s.st.Accounts(); err == nil {
		for _, a := range accts {
			if a.Kind == "wallet" {
				n++
			}
		}
	}
	s.recheck.mu.Lock()
	s.recheck.st = recheckState{Total: n, Done: n, Errors: []string{}, Finished: time.Now().UTC().Format(time.RFC3339)}
	s.recheck.mu.Unlock()
	s.recheckStatus(w, r)
}

func (s *Server) demoLightningSync(w http.ResponseWriter, r *http.Request) {
	evs, err := s.st.LightningEvents()
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.st.SetSetting("clnSynced", time.Now().UTC().Format(time.RFC3339))
	writeJSON(w, map[string]any{"alias": s.st.Setting("clnAlias", ""), "network": "bitcoin", "events": len(evs), "added": 0})
}

// demoSettings drops settings a demo copy keeps fixed: its simulated node,
// Lightning node and assistant.
func demoSettings(in map[string]string) {
	for k := range in {
		for _, p := range []string{"fulcrum", "regtest", "cln", "ai"} {
			if strings.HasPrefix(k, p) {
				delete(in, k)
			}
		}
	}
}
