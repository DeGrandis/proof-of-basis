// Package app is the HTTP API and static UI.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"btctax/internal/electrum"
	"btctax/internal/importers"
	"btctax/internal/lots"
	"btctax/internal/ollama"
	"btctax/internal/prices"
	"btctax/internal/store"
	"btctax/internal/tax"
	"btctax/internal/trace"
	"btctax/internal/wallet"
)

type Server struct {
	st      *store.Store
	static  fs.FS
	scanMu  sync.Mutex
	authMu  sync.Mutex
	limit   limiter
	bundled map[string]int64

	statusMu sync.Mutex
	status   fulcrumStatus

	recheck rechecker
}

type fulcrumStatus struct {
	OK      bool      `json:"ok"`
	Height  int64     `json:"height,omitempty"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked"`
	host    string
}

func New(st *store.Store, static fs.FS) *Server {
	return &Server{st: st, static: static, bundled: prices.Bundled()}
}

var defaults = map[string]string{
	"fulcrumHost":  "",
	"fulcrumPort":  "50002",
	"fulcrumTLS":   "true",
	"fulcrumCert":  "",
	"regtest":      "false",
	"jurisdiction": "us",
	"method":       string(tax.FIFO),
	// IRS FAQs 81 and 97 (2025): BTC spent on a network fee, even moving
	// coins between your own wallets, is a disposal of those coins.
	"feeMode":      string(lots.FeeAsDisposal),
	"timezone":     "UTC",
	"filingStatus": "single",
	"rateShort":    "24",
	"rateLong":     "15",
	"priceSource":  "yahoo",
	"clnUrl":       "",
	"clnCert":      "",
	"clnName":      "Lightning node",
	"aiUrl":        "",
	"aiModel":      "",
	"aiCtx":        "16384",
	"aiThink":      "true",
	"aiAutoApply":  "false",
}

func (s *Server) settings() map[string]string {
	m := map[string]string{}
	for k, d := range defaults {
		m[k] = s.st.Setting(k, d)
	}
	return m
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("POST /api/settings", s.postSettings)
	mux.HandleFunc("POST /api/fulcrum/test", s.testFulcrum)
	mux.HandleFunc("GET /api/fulcrum/status", s.fulcrumStatus)
	mux.HandleFunc("POST /api/wallets/preview", s.previewWallet)
	mux.HandleFunc("POST /api/wallets", s.addWallet)
	mux.HandleFunc("POST /api/wallets/{id}/scan", s.noted("scan", "Scanned a wallet on your node", s.scanWallet))
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("GET /api/handlers", s.listHandlers)
	mux.HandleFunc("GET /api/imports", s.listImports)
	mux.HandleFunc("GET /api/imports/freshness", s.importFreshness)
	mux.HandleFunc("POST /api/imports", s.postImport)
	mux.HandleFunc("DELETE /api/imports/{id}", s.deleteImport)
	mux.HandleFunc("GET /api/report", s.getReport)
	mux.HandleFunc("POST /api/labels", s.postLabel)
	mux.HandleFunc("DELETE /api/labels/{key}", s.deleteLabel)
	mux.HandleFunc("POST /api/prices", s.noted("prices", "Imported a price history file", s.postPrices))
	mux.HandleFunc("GET /api/prices", s.priceInfo)
	mux.HandleFunc("POST /api/prices/fetch", s.noted("prices", "Updated BTC prices", s.fetchPrices))
	mux.HandleFunc("GET /api/export/8949.csv", s.export8949)
	mux.HandleFunc("POST /api/wipe", s.wipe)
	mux.HandleFunc("POST /api/whatif", s.whatIf)
	mux.HandleFunc("POST /api/protect", s.protect)
	mux.HandleFunc("GET /api/tx/{txid}", s.getTx)
	mux.HandleFunc("GET /api/address/{addr}", s.getAddress)
	mux.HandleFunc("POST /api/recheck", s.noted("recheck", "Rechecked wallets for links", s.startRecheck))
	mux.HandleFunc("POST /api/lightning/sync", s.noted("lightning", "Synced the Lightning node", s.syncLightning))
	mux.HandleFunc("POST /api/backup", s.postBackup)
	mux.HandleFunc("POST /api/restore", s.noted("restore", "Restored from a backup (replaced all data; can't be undone here)", s.postRestore))
	mux.HandleFunc("GET /api/recheck", s.recheckStatus)
	mux.HandleFunc("GET /api/ai/status", s.aiStatus)
	mux.HandleFunc("POST /api/ai/models", s.aiModels)
	mux.HandleFunc("GET /api/ai/chats", s.aiChats)
	mux.HandleFunc("POST /api/ai/chats", s.aiNewChat)
	mux.HandleFunc("DELETE /api/ai/chats", s.aiDeleteChats)
	mux.HandleFunc("GET /api/ai/chats/{id}", s.aiGetChat)
	mux.HandleFunc("POST /api/ai/chats/{id}/rename", s.aiRenameChat)
	mux.HandleFunc("DELETE /api/ai/chats/{id}", s.aiDeleteChat)
	mux.HandleFunc("POST /api/ai/chats/{id}/send", s.aiSend)
	mux.HandleFunc("GET /api/ai/export/{token}", s.getExport)
	mux.HandleFunc("POST /api/labels/bulk", s.bulkLabel)
	mux.HandleFunc("GET /api/actions", s.listActions)
	mux.HandleFunc("GET /api/actions/{id}", s.getAction)
	mux.HandleFunc("POST /api/actions/{id}/undo", s.undoAction)
	mux.HandleFunc("POST /api/actions/{id}/redo", s.redoAction)
	mux.HandleFunc("POST /api/actions/{id}/apply", s.applyAction)
	mux.HandleFunc("POST /api/actions/{id}/dismiss", s.dismissAction)
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/setup", s.authSetup)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/logout", s.authLogout)
	mux.HandleFunc("POST /api/auth/logout-all", s.authLogoutAll)
	mux.HandleFunc("POST /api/auth/password", s.authPassword)
	mux.Handle("GET /", http.FileServerFS(s.static))
	return s.secure(s.requireLogin(mux))
}

// secure adds a strict CSP (no third-party anything) and refuses
// cross-site writes. Login is checked inside it, by requireLogin.
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		// Reject cross-site writes (CSRF) from other pages in the browser.
		if r.Method != http.MethodGet && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// The node's rune is write-only: it is saved but never sent back.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	m := s.settings()
	m["clnRuneSet"] = strconv.FormatBool(s.st.Setting("clnRune", "") != "")
	writeJSON(w, m)
}

func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	// Log what this changes (never the rune itself).
	logKeys := []string{"clnUrl", "clnCert", "fulcrumCert"}
	for k := range in {
		logKeys = append(logKeys, k)
	}
	sort.Strings(logKeys)
	pre, err := s.snapshot(settingChanges(logKeys))
	if err != nil {
		fail(w, 500, err)
		return
	}
	if rn := strings.TrimSpace(in["clnRune"]); rn != "" {
		defer s.note("lightning", "Saved a Lightning rune (not shown here)")
	}
	if _, ok := in["clnForget"]; ok {
		defer s.note("lightning", "Disconnected the Lightning node and forgot its rune")
	}
	if rn, ok := in["clnRune"]; ok {
		delete(in, "clnRune")
		if rn = strings.TrimSpace(rn); rn != "" {
			if err := s.st.SetSetting("clnRune", rn); err != nil {
				fail(w, 500, err)
				return
			}
		}
	}
	if _, ok := in["clnForget"]; ok {
		delete(in, "clnForget")
		for _, k := range []string{"clnRune", "clnUrl", "clnCert", "clnChannels", "clnSynced", "clnAlias"} {
			s.st.SetSetting(k, "")
		}
	}
	delete(in, "clnRuneSet")
	for k, v := range in {
		if _, ok := defaults[k]; !ok && !strings.HasPrefix(k, "method:") {
			fail(w, 400, fmt.Errorf("unknown setting %q", k))
			return
		}
		switch k {
		case "method":
			if !validMethod(v) {
				fail(w, 400, fmt.Errorf("method must be fifo, lifo or specific"))
				return
			}
		case "priceSource":
			if _, ok := prices.Get(v); !ok {
				fail(w, 400, fmt.Errorf("unknown price source %q", v))
				return
			}
		case "feeMode":
			if v != string(lots.FeeCarry) && v != string(lots.FeeAsDisposal) {
				fail(w, 400, fmt.Errorf("feeMode must be carry or dispose"))
				return
			}
		case "timezone":
			if _, err := time.LoadLocation(v); err != nil || v == "" {
				fail(w, 400, fmt.Errorf("unknown time zone %q; use a name like America/New_York", v))
				return
			}
		case "filingStatus":
			if _, ok := lossLimit[v]; !ok {
				fail(w, 400, errors.New("filingStatus must be single, mfj, mfs or hoh"))
				return
			}
		case "aiUrl":
			if v = strings.TrimSpace(v); v != "" {
				if _, err := ollama.New(v); err != nil {
					fail(w, 400, err)
					return
				}
			}
			in[k] = v
		case "aiThink", "aiAutoApply":
			if v != "true" && v != "false" {
				fail(w, 400, errors.New("aiThink must be true or false"))
				return
			}
		case "aiCtx":
			if n, err := strconv.Atoi(v); err != nil || n < 2048 || n > 262144 {
				fail(w, 400, errors.New("context size must be between 2048 and 262144 tokens"))
				return
			}
		case "clnUrl":
			if s.st.Setting(k, "") != strings.TrimSpace(v) {
				s.st.SetSetting("clnCert", "")
			}
		case "fulcrumHost", "fulcrumPort", "fulcrumTLS":
			// A new server means a new certificate to pin.
			if s.st.Setting(k, defaults[k]) != v {
				s.st.SetSetting("fulcrumCert", "")
			}
		}
		if strings.HasPrefix(k, "method:") && v != "" && !validMethod(v) {
			fail(w, 400, fmt.Errorf("bad method %q", v))
			return
		}
		if err := s.st.SetSetting(k, strings.TrimSpace(v)); err != nil {
			fail(w, 500, err)
			return
		}
	}
	s.finish("user", "settings", "Changed settings", 0, pre)
	writeJSON(w, s.settings())
}

func validMethod(v string) bool {
	for _, m := range (tax.US{}).Methods() {
		if string(m) == v {
			return true
		}
	}
	return false
}

func (s *Server) dial(ctx context.Context) (*electrum.Client, error) {
	set := s.settings()
	if set["fulcrumHost"] == "" {
		return nil, errors.New("set your Fulcrum server address first")
	}
	port, err := strconv.Atoi(set["fulcrumPort"])
	if err != nil {
		return nil, fmt.Errorf("bad Fulcrum port: %w", err)
	}
	c, err := electrum.Dial(ctx, electrum.Config{Host: set["fulcrumHost"], Port: port, TLS: set["fulcrumTLS"] == "true",
		PinnedCertSHA256: set["fulcrumCert"]})
	if err != nil {
		return nil, err
	}
	if c.CertSHA256 != "" && set["fulcrumCert"] == "" {
		s.st.SetSetting("fulcrumCert", c.CertSHA256) // trust on first use
	}
	return c, nil
}

func (s *Server) testFulcrum(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := s.dial(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	defer c.Close()
	tip, err := c.Tip()
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]any{"height": tip.Height, "cert": c.CertSHA256})
}

// fulcrumStatus reports whether the Fulcrum server answers, for the status
// dot in the menu. Results are reused for 30 seconds so polling from
// several tabs does not hammer the server.
func (s *Server) fulcrumStatus(w http.ResponseWriter, r *http.Request) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	host := s.st.Setting("fulcrumHost", "") + ":" + s.st.Setting("fulcrumPort", "") + ":" + s.st.Setting("fulcrumTLS", "")
	if s.status.host == host && time.Since(s.status.Checked) < 30*time.Second && r.URL.Query().Get("fresh") == "" {
		writeJSON(w, s.status)
		return
	}
	st := fulcrumStatus{Checked: time.Now().UTC(), host: host}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if c, err := s.dial(ctx); err != nil {
		st.Error = err.Error()
	} else {
		if tip, err := c.Tip(); err != nil {
			st.Error = err.Error()
		} else {
			st.OK, st.Height = true, tip.Height
		}
		c.Close()
	}
	s.status = st
	writeJSON(w, st)
}

type walletIn struct {
	Name     string      `json:"name"`
	Key      string      `json:"key"`
	Script   string      `json:"script"`
	Gap      int         `json:"gap"`
	Multisig *multisigIn `json:"multisig,omitempty"`
}

// multisigIn describes a multisig wallet by its cosigner keys; it is turned
// into an output descriptor.
type multisigIn struct {
	Threshold int      `json:"threshold"`
	Keys      []string `json:"keys"`
	Script    string   `json:"script"` // p2wsh, p2sh-p2wsh, p2sh; empty = from key prefix
	Sorted    bool     `json:"sorted"`
}

func (s *Server) parseWallet(in walletIn) (wallet.Deriver, error) {
	return wallet.Parse(in.Key, wallet.ScriptType(in.Script), s.st.Setting("regtest", "false") == "true")
}

// resolveMultisig replaces a multisig form with the descriptor it describes.
func resolveMultisig(in *walletIn) error {
	if in.Multisig == nil {
		return nil
	}
	d, err := wallet.MultisigDescriptor(in.Multisig.Threshold, in.Multisig.Keys, in.Multisig.Script, in.Multisig.Sorted)
	if err != nil {
		return err
	}
	in.Key, in.Script, in.Multisig = d, "", nil
	return nil
}

func describe(d wallet.Deriver) (kind, script string) {
	switch v := d.(type) {
	case *wallet.Account:
		return v.Prefix, string(v.Script)
	case *wallet.Descriptor:
		return "descriptor", v.Kind()
	}
	return "", ""
}

// previewWallet shows the first addresses so the user can check them
// against their wallet software before saving.
func (s *Server) previewWallet(w http.ResponseWriter, r *http.Request) {
	var in walletIn
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if err := resolveMultisig(&in); err != nil {
		fail(w, 400, err)
		return
	}
	acct, err := s.parseWallet(in)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var addrs []string
	for i := uint32(0); i < 3; i++ {
		a, err := acct.Derive(0, i)
		if err != nil {
			fail(w, 400, err)
			return
		}
		addrs = append(addrs, a.Address)
	}
	kind, script := describe(acct)
	writeJSON(w, map[string]any{"script": script, "prefix": kind, "addresses": addrs, "descriptor": in.Key})
}

func (s *Server) addWallet(w http.ResponseWriter, r *http.Request) {
	var in walletIn
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		fail(w, 400, errors.New("give the wallet a name"))
		return
	}
	if err := resolveMultisig(&in); err != nil {
		fail(w, 400, err)
		return
	}
	acct, err := s.parseWallet(in)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if in.Gap <= 0 {
		in.Gap = 20
	}
	if dup, err := s.findOverlap(acct); err != nil {
		fail(w, 500, err)
		return
	} else if dup != "" {
		fail(w, 409, fmt.Errorf("this wallet is already added as %q", dup))
		return
	}
	if accts, err := s.st.Accounts(); err == nil {
		for _, a := range accts {
			if strings.EqualFold(a.Name, in.Name) {
				fail(w, 409, fmt.Errorf("there is already an account named %q", a.Name))
				return
			}
		}
	}
	_, script := describe(acct)
	if _, ok := acct.(*wallet.Descriptor); ok {
		in.Script = "" // the descriptor itself defines the script
	} else {
		in.Script = script
	}
	id, err := s.st.AddWallet(in.Name, strings.TrimSpace(in.Key), in.Script, min(in.Gap, 1000))
	if err != nil {
		fail(w, 400, err)
		return
	}
	s.logAdded("wallet", "Added wallet "+in.Name, accountSets(id))
	writeJSON(w, map[string]any{"id": id})
}

// findOverlap returns the name of an existing wallet that derives any of
// the new wallet's first receive or change addresses: the same key added
// twice, or the same key as an xpub and as a descriptor.
func (s *Server) findOverlap(d wallet.Deriver) (string, error) {
	const n = 5
	mine := map[string]bool{}
	for chain := uint32(0); chain < d.Chains(); chain++ {
		for i := uint32(0); i < n; i++ {
			a, err := d.Derive(chain, i)
			if err != nil {
				return "", err
			}
			mine[a.Address] = true
		}
	}
	accts, err := s.st.Accounts()
	if err != nil {
		return "", err
	}
	for _, a := range accts {
		if a.Kind != "wallet" {
			continue
		}
		other, err := s.parseWallet(walletIn{Key: a.XPub, Script: a.Script})
		if err != nil {
			continue // e.g. saved under the other network setting
		}
		for chain := uint32(0); chain < other.Chains(); chain++ {
			for i := uint32(0); i < n; i++ {
				if x, err := other.Derive(chain, i); err == nil && mine[x.Address] {
					return a.Name, nil
				}
			}
		}
	}
	return "", nil
}

// protect marks an acquisition (by its record key) to be sold last, or
// clears the mark.
func (s *Server) protect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
		On  bool   `json:"on"`
	}
	if err := decode(r, &in); err != nil || in.Key == "" || strings.ContainsAny(in.Key, "\n\r") {
		fail(w, 400, errors.New("key is required"))
		return
	}
	summary := map[bool]string{true: "Protected a purchase", false: "Stopped protecting a purchase"}[in.On]
	if _, err := s.record("user", "protect", summary, 0, []change{{Kind: "protect", Key: in.Key}}, func() error { return s.setProtected(in.Key, in.On) }); err != nil {
		fail(w, 500, err)
		return
	}
	ids := make([]string, 0)
	for id := range s.protected() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	writeJSON(w, map[string]any{"protected": ids})
}

// whatIf estimates the tax on selling an amount from one account today, at
// the latest price, with the user's method and protected lots honoured. It
// changes nothing.
func (s *Server) whatIf(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Account string   `json:"account"`
		BTC     float64  `json:"btc"`
		USD     float64  `json:"usd"`
		Lots    []string `json:"lots"` // specific lots to sell, by open-lot id
		// Strategy picks the coins: highest_cost, lowest_cost, oldest,
		// newest or long_term_first (used when Lots is empty).
		Strategy string `json:"strategy"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	p, err := s.pipeline()
	if err != nil {
		fail(w, 500, err)
		return
	}
	day, price, ok := p.prices.latest()
	if !ok || price <= 0 {
		fail(w, 400, errors.New("no BTC price yet: press Update prices in Settings"))
		return
	}
	sats := int64(in.BTC*1e8 + 0.5)
	if in.USD > 0 {
		sats = lots.MulDiv(int64(in.USD*100+0.5), 100_000_000, price)
	}
	base, err := lots.Run(p.tr.Ops, p.cfg)
	if err != nil {
		fail(w, 500, err)
		return
	}
	// Picking coins: sell exactly those lots, whatever the year's method.
	var picks []lots.Pick
	if len(in.Lots) == 0 && in.Strategy != "" && sats > 0 {
		var held []lots.Lot
		for _, l := range base.Open {
			if l.Account == in.Account && l.Sats > 0 {
				held = append(held, l)
			}
		}
		now := time.Now()
		cost := func(l lots.Lot) float64 { return float64(l.Basis) / float64(l.Sats) }
		long := func(l lots.Lot) bool { return now.Sub(l.Acquired) > 365*24*time.Hour }
		switch in.Strategy {
		case "highest_cost":
			sort.SliceStable(held, func(i, j int) bool { return cost(held[i]) > cost(held[j]) })
		case "lowest_cost":
			sort.SliceStable(held, func(i, j int) bool { return cost(held[i]) < cost(held[j]) })
		case "oldest":
			sort.SliceStable(held, func(i, j int) bool { return held[i].Acquired.Before(held[j].Acquired) })
		case "newest":
			sort.SliceStable(held, func(i, j int) bool { return held[i].Acquired.After(held[j].Acquired) })
		case "long_term_first":
			sort.SliceStable(held, func(i, j int) bool {
				if long(held[i]) != long(held[j]) {
					return long(held[i])
				}
				return cost(held[i]) > cost(held[j])
			})
		default:
			fail(w, 400, errors.New("strategy must be highest_cost, lowest_cost, oldest, newest or long_term_first"))
			return
		}
		left := sats
		for _, l := range held {
			if left <= 0 {
				break
			}
			take := min(l.Sats, left)
			picks = append(picks, lots.Pick{LotID: l.ID, Sats: take})
			left -= take
		}
		if left > 0 {
			fail(w, 400, fmt.Errorf("%s holds %s BTC, less than that", in.Account, satsStr(sats-left)))
			return
		}
		cfg := p.cfg
		yearMethod := cfg.Method
		cfg.Method = func(y int) tax.Method {
			if y == time.Now().In(loc(p.set)).Year() {
				return tax.Specific
			}
			return yearMethod(y)
		}
		p.cfg = cfg
	}
	if len(in.Lots) > 0 {
		want := map[string]bool{}
		for _, id := range in.Lots {
			want[id] = true
		}
		sats = 0
		for _, l := range base.Open {
			if want[l.ID] {
				if l.Account != in.Account {
					fail(w, 400, errors.New("pick coins from one account at a time"))
					return
				}
				picks = append(picks, lots.Pick{LotID: l.ID, Sats: l.Sats})
				sats += l.Sats
			}
		}
		if len(picks) != len(want) {
			fail(w, 400, errors.New("some picked coins no longer exist; refresh and pick again"))
			return
		}
		cfg := p.cfg
		yearMethod := cfg.Method
		cfg.Method = func(y int) tax.Method {
			if y == time.Now().In(loc(p.set)).Year() {
				return tax.Specific
			}
			return yearMethod(y)
		}
		p.cfg = cfg
	}
	if sats <= 0 || in.Account == "" {
		fail(w, 400, errors.New("enter an amount and pick an account"))
		return
	}
	if have := base.Balances[in.Account]; sats > have {
		fail(w, 400, fmt.Errorf("%s holds %s BTC, less than that", in.Account, satsStr(have)))
		return
	}
	now := time.Now().In(loc(p.set))
	ops := append(append([]lots.Op(nil), p.tr.Ops...), lots.Op{ID: "whatif", Kind: lots.Dispose, Time: now, Account: in.Account,
		Sats: sats, Cents: lots.MulDiv(price, sats, 100_000_000), Picks: picks})
	res, err := lots.Run(ops, p.cfg)
	if err != nil {
		fail(w, 500, err)
		return
	}
	type side struct {
		Sats     int64 `json:"sats"`
		Proceeds int64 `json:"proceeds"`
		Basis    int64 `json:"basis"`
	}
	var short, long side
	var missing int64
	for _, d := range res.Disposals {
		if d.OpID != "whatif" {
			continue
		}
		sd := &short
		if d.LongTerm {
			sd = &long
		}
		sd.Sats += d.Sats
		sd.Proceeds += d.Proceeds
		sd.Basis += d.Basis
		if d.Missing {
			missing += d.Sats
		}
	}
	var warns []string
	for _, wv := range res.Warnings {
		if wv.OpID == "whatif" {
			warns = append(warns, wv.Message)
		}
	}
	// The extra tax this sale causes: this year's impact with and without
	// it, with earlier years' carried losses applied.
	thisYear := func(ys []YearSummary) int64 {
		for _, y := range ys {
			if y.Year == now.Year() {
				return y.EstimatedTax
			}
		}
		return 0
	}
	before := thisYear(summarize(base.Disposals, p.tr.Income, p.set, now.Year()))
	after := thisYear(summarize(res.Disposals, p.tr.Income, p.set, now.Year()))
	writeJSON(w, map[string]any{"sats": sats, "price": price, "priceDay": day, "proceeds": short.Proceeds + long.Proceeds,
		"short": short, "long": long, "missingSats": missing, "extraTax": after - before, "warnings": warns,
		"method": string(p.cfg.Method(now.Year()))})
}

// wipe deletes all data. The body must be {"confirm":"WIPE"} so a stray
// request cannot do it.
func (s *Server) wipe(w http.ResponseWriter, r *http.Request) {
	var in struct{ Confirm string }
	if err := decode(r, &in); err != nil || in.Confirm != "WIPE" {
		fail(w, 400, errors.New(`send {"confirm":"WIPE"} to delete everything`))
		return
	}
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if err := s.st.Wipe(); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// cachedSource serves raw transactions and headers from SQLite when it can.
type cachedSource struct {
	c  *electrum.Client
	st *store.Store
}

func (c cachedSource) History(sh string) ([]electrum.HistoryItem, error) { return c.c.History(sh) }
func (c cachedSource) RawTx(id string) (string, error) {
	if h, ok := c.st.CachedTx(id); ok {
		return h, nil
	}
	return c.c.RawTx(id)
}
func (c cachedSource) Header(h int64) (string, error) {
	if x, ok := c.st.CachedHeader(h); ok {
		return x, nil
	}
	x, err := c.c.Header(h)
	if err == nil {
		c.st.PutHeader(h, x)
	}
	return x, err
}

func (s *Server) scanWallet(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if a, err := s.st.Account(id); err != nil || a.Kind != "wallet" {
		fail(w, 404, errors.New("no such wallet"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.rescan(ctx, id)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]any{"balance": res.Balance, "txs": len(res.Txs), "usedAddresses": len(res.Used)})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name := fmt.Sprint(id)
	if a, err := s.st.Account(id); err == nil {
		name = a.Name
	}
	if _, err := s.record("user", "account", "Deleted account "+name, 0, []change{{Kind: "rows", Key: "Deleted account " + name + " with its imports and scans", Sets: accountSets(id)}},
		func() error { return s.st.DeleteAccount(id) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) listHandlers(w http.ResponseWriter, r *http.Request) {
	var out []map[string]string
	for _, h := range importers.All() {
		out = append(out, map[string]string{"id": h.ID(), "name": h.Name()})
	}
	writeJSON(w, out)
}

func (s *Server) listImports(w http.ResponseWriter, r *http.Request) {
	im, err := s.st.Imports()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, im)
}

// importFreshness gives, for each exchange account, the date of its newest
// imported row, so the UI can suggest downloading a fresh export.
func (s *Server) importFreshness(w http.ResponseWriter, r *http.Request) {
	ex, err := s.st.ExchangeEvents()
	if err != nil {
		fail(w, 500, err)
		return
	}
	last := map[string]time.Time{}
	for _, e := range ex {
		if e.Time.After(last[e.Account]) {
			last[e.Account] = e.Time
		}
	}
	type row struct {
		Account string `json:"account"`
		Last    string `json:"last"`
		Days    int    `json:"days"`
	}
	out := []row{}
	for a, t := range last {
		out = append(out, row{a, t.UTC().Format("2006-01-02"), int(time.Since(t).Hours() / 24)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Days > out[j].Days })
	writeJSON(w, out)
}

func (s *Server) postImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(w, 400, err)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		fail(w, 400, err)
		return
	}
	h, evs, warns, err := importers.Import(strings.NewReader(string(body)), r.FormValue("handler"))
	if errors.Is(err, importers.ErrUnknownFormat) {
		// 422 tells the UI to ask which platform the file is from.
		fail(w, http.StatusUnprocessableEntity, err)
		return
	} else if err != nil {
		fail(w, 400, err)
		return
	}
	name := strings.TrimSpace(r.FormValue("account"))
	if name == "" {
		name = h.Name()
	}
	_, existed := s.st.AccountByName(name)
	acctID, err := s.st.ExchangeAccount(name, h.ID())
	if err != nil {
		fail(w, 400, err)
		return
	}
	sum := sha256.Sum256(body)
	id, skipped, err := s.st.AddImport(acctID, h.ID(), hdr.Filename, hex.EncodeToString(sum[:]), evs, warns)
	if errors.Is(err, store.ErrDuplicate) {
		fail(w, 409, err)
		return
	} else if err != nil {
		fail(w, 500, err)
		return
	}
	sets := importSets(id)
	if !existed {
		sets = append([]store.RowSet{{Table: "accounts", Col: "id", Val: acctID}}, sets...)
	}
	s.logAdded("import", fmt.Sprintf("Imported %s into %s (%d records)", hdr.Filename, name, len(evs)-skipped), sets)
	writeJSON(w, map[string]any{"id": id, "handler": h.ID(), "account": name, "events": len(evs) - skipped, "alreadyImported": skipped, "warnings": warns})
}

func (s *Server) deleteImport(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	what := "an import"
	if ims, err := s.st.Imports(); err == nil {
		for _, im := range ims {
			if im.ID == id {
				what = im.Filename + " from " + im.Account
			}
		}
	}
	if _, err := s.record("user", "import", "Deleted import "+what, 0, []change{{Kind: "rows", Key: "Deleted import " + what + " and its records", Sets: importSets(id)}},
		func() error { return s.st.DeleteImport(id) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) postLabel(w http.ResponseWriter, r *http.Request) {
	var l trace.Label
	if err := decode(r, &l); err != nil {
		fail(w, 400, err)
		return
	}
	if l.Key == "" || l.Category == "" {
		fail(w, 400, errors.New("key and category are required"))
		return
	}
	if _, err := s.record("user", "label", "Labeled a record", 0, []change{{Kind: "label", Key: l.Key}}, func() error { return s.st.SetLabel(l) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, l)
}

func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if _, err := s.record("user", "label", "Removed a label", 0, []change{{Kind: "label", Key: key}}, func() error { return s.st.DeleteLabel(key) }); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// postPrices imports a CSV of "date,price" rows (USD per BTC).
func (s *Server) postPrices(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		fail(w, 400, err)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer f.Close()
	t, err := importers.ReadTable(f)
	if err != nil {
		fail(w, 400, err)
		return
	}
	di, pi := t.Col("date", "day", "timestamp", "time"), t.Col("close", "price", "usd", "close (usd)", "price (usd)")
	if di < 0 || pi < 0 {
		fail(w, 400, errors.New("expected a date column and a close/price column"))
		return
	}
	out := map[string]int64{}
	for _, row := range t.Rows {
		if di >= len(row) || pi >= len(row) {
			continue
		}
		d, err1 := importers.ParseTime(row[di])
		c, err2 := importers.Cents(row[pi])
		if err1 == nil && err2 == nil && c > 0 {
			out[d.Format("2006-01-02")] = c
		}
	}
	if err := s.st.PutPrices(out, "import"); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]int{"days": len(out)})
}

// export8949 writes a Form 8949-style CSV for one tax year.
func (s *Server) export8949(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="form8949-%d.csv"`, year))
	cw := csv.NewWriter(w)
	cw.Write([]string{"Description", "Date acquired", "Date sold", "Proceeds", "Cost basis", "Gain or loss", "Term", "Account", "Source lot", "Note"})
	money := func(c int64) string {
		sign := ""
		if c < 0 {
			sign, c = "-", -c
		}
		return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
	}
	for _, d := range rep.Disposals {
		if d.Disposed.Year() != year {
			continue
		}
		term, acq, note := "Short", d.Acquired.Format("01/02/2006"), ""
		if d.LongTerm {
			term = "Long"
		}
		if d.Missing {
			acq, note = "VARIOUS", "basis unknown: missing records"
		}
		cw.Write([]string{fmt.Sprintf("%s BTC", satsStr(d.Sats)), acq, d.Disposed.Format("01/02/2006"),
			money(d.Proceeds), money(d.Basis), money(d.Gain()), term, d.Account, d.OriginID, note})
	}
	cw.Flush()
}

func satsStr(s int64) string { return fmt.Sprintf("%d.%08d", s/100_000_000, s%100_000_000) }

func LogRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		// Paths only: never log bodies, keys, addresses or amounts.
		p := r.URL.Path
		for _, pre := range []string{"/api/labels/", "/api/tx/", "/api/address/"} {
			if strings.HasPrefix(p, pre) {
				p = pre + "…"
			}
		}
		log.Printf("%s %s %s", r.Method, p, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) priceInfo(w http.ResponseWriter, r *http.Request) {
	stored, err := s.st.Prices()
	if err != nil {
		fail(w, 500, err)
		return
	}
	span := func(m map[string]int64) map[string]any {
		first, last := "", ""
		for d := range m {
			if first == "" || d < first {
				first = d
			}
			if d > last {
				last = d
			}
		}
		return map[string]any{"days": len(m), "first": first, "last": last}
	}
	var srcs []map[string]string
	for _, p := range prices.All() {
		srcs = append(srcs, map[string]string{"id": p.ID(), "name": p.Name(), "host": p.Host()})
	}
	writeJSON(w, map[string]any{"bundled": span(s.bundled), "stored": span(stored), "sources": srcs})
}

// fetchPrices runs only when the user presses the button: it is the one
// place the running app contacts anything other than their own node.
func (s *Server) fetchPrices(w http.ResponseWriter, r *http.Request) {
	src, ok := prices.Get(s.st.Setting("priceSource", defaults["priceSource"]))
	if !ok {
		fail(w, 400, errors.New("no price source configured"))
		return
	}
	stored, err := s.st.Prices()
	if err != nil {
		fail(w, 500, err)
		return
	}
	last := ""
	for _, m := range []map[string]int64{s.bundled, stored} {
		for d := range m {
			if d > last {
				last = d
			}
		}
	}
	from := time.Date(2010, 7, 17, 0, 0, 0, 0, time.UTC)
	if t, err := time.Parse("2006-01-02", last); err == nil {
		from = t.AddDate(0, 0, -7)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	got, err := src.Fetch(ctx, from, time.Now())
	if err != nil {
		fail(w, 502, err)
		return
	}
	if err := s.st.PutPrices(got, src.ID()); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"days": len(got), "source": src.Name()})
}
