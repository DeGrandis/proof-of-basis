// Package app is the HTTP API and static UI.
package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
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
	"btctax/internal/prices"
	"btctax/internal/scan"
	"btctax/internal/store"
	"btctax/internal/tax"
	"btctax/internal/trace"
	"btctax/internal/wallet"
)

type Server struct {
	st       *store.Store
	static   fs.FS
	password string
	scanMu   sync.Mutex
	bundled  map[string]int64

	statusMu sync.Mutex
	status   fulcrumStatus
}

type fulcrumStatus struct {
	OK      bool      `json:"ok"`
	Height  int64     `json:"height,omitempty"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked"`
	host    string
}

func New(st *store.Store, static fs.FS, password string) *Server {
	return &Server{st: st, static: static, password: password, bundled: prices.Bundled()}
}

var defaults = map[string]string{
	"fulcrumHost":  "",
	"fulcrumPort":  "50002",
	"fulcrumTLS":   "true",
	"fulcrumCert":  "",
	"regtest":      "false",
	"jurisdiction": "us",
	"method":       string(tax.FIFO),
	"feeMode":      string(lots.FeeCarry),
	"rateShort":    "24",
	"rateLong":     "15",
	"priceSource":  "yahoo",
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
	mux.HandleFunc("POST /api/wallets/{id}/scan", s.scanWallet)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("GET /api/handlers", s.listHandlers)
	mux.HandleFunc("GET /api/imports", s.listImports)
	mux.HandleFunc("GET /api/imports/freshness", s.importFreshness)
	mux.HandleFunc("POST /api/imports", s.postImport)
	mux.HandleFunc("DELETE /api/imports/{id}", s.deleteImport)
	mux.HandleFunc("GET /api/report", s.getReport)
	mux.HandleFunc("POST /api/labels", s.postLabel)
	mux.HandleFunc("DELETE /api/labels/{key}", s.deleteLabel)
	mux.HandleFunc("POST /api/prices", s.postPrices)
	mux.HandleFunc("GET /api/prices", s.priceInfo)
	mux.HandleFunc("POST /api/prices/fetch", s.fetchPrices)
	mux.HandleFunc("GET /api/export/8949.csv", s.export8949)
	mux.HandleFunc("POST /api/wipe", s.wipe)
	mux.HandleFunc("POST /api/whatif", s.whatIf)
	mux.HandleFunc("POST /api/protect", s.protect)
	mux.Handle("GET /", http.FileServerFS(s.static))
	return s.secure(mux)
}

// secure adds a strict CSP (no third-party anything) and optional password.
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if s.password != "" {
			_, pw, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(s.password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="btctax"`)
				http.Error(w, "unauthorised", http.StatusUnauthorized)
				return
			}
		}
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

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.settings()) }

func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
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
	set := s.protected()
	if in.On {
		set[in.Key] = true
	} else {
		delete(set, in.Key)
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if err := s.st.SetSetting("protected", strings.Join(ids, "\n")); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"protected": ids})
}

// whatIf estimates the tax on selling an amount from one account today, at
// the latest price, with the user's method and protected lots honoured. It
// changes nothing.
func (s *Server) whatIf(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Account string  `json:"account"`
		BTC     float64 `json:"btc"`
		USD     float64 `json:"usd"`
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
	if sats <= 0 || in.Account == "" {
		fail(w, 400, errors.New("enter an amount and pick an account"))
		return
	}
	base, err := lots.Run(p.tr.Ops, p.cfg)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if have := base.Balances[in.Account]; sats > have {
		fail(w, 400, fmt.Errorf("%s holds %s BTC, less than that", in.Account, satsStr(have)))
		return
	}
	now := time.Now().UTC()
	ops := append(append([]lots.Op(nil), p.tr.Ops...), lots.Op{ID: "whatif", Kind: lots.Dispose, Time: now, Account: in.Account,
		Sats: sats, Cents: lots.MulDiv(price, sats, 100_000_000)})
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
	// The year's other results, to estimate the extra tax this sale causes.
	var yst, ylt int64
	for _, d := range base.Disposals {
		if d.Disposed.Year() == now.Year() {
			if d.LongTerm {
				ylt += d.Gain()
			} else {
				yst += d.Gain()
			}
		}
	}
	for _, inc := range p.tr.Income {
		if inc.Time.Year() == now.Year() {
			yst += inc.Cents
		}
	}
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
	sbp, lbp := rateBP(p.set, "rateShort"), rateBP(p.set, "rateLong")
	before := yearTax(yst, ylt, sbp, lbp)
	after := yearTax(yst+short.Proceeds-short.Basis, ylt+long.Proceeds-long.Basis, sbp, lbp)
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
	a, err := s.st.Account(id)
	if err != nil || a.Kind != "wallet" {
		fail(w, 404, errors.New("no such wallet"))
		return
	}
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	acct, err := s.parseWallet(walletIn{Key: a.XPub, Script: a.Script})
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	c, err := s.dial(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	defer c.Close()
	res, err := scan.Wallet(cachedSource{c, s.st}, acct, a.Gap)
	if err != nil {
		fail(w, 502, err)
		return
	}
	if err := s.st.SaveScan(id, res); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"balance": res.Balance, "txs": len(res.Txs), "usedAddresses": len(res.Used)})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteAccount(id); err != nil {
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
	writeJSON(w, map[string]any{"id": id, "handler": h.ID(), "account": name, "events": len(evs) - skipped, "alreadyImported": skipped, "warnings": warns})
}

func (s *Server) deleteImport(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteImport(id); err != nil {
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
	if err := s.st.SetLabel(l); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, l)
}

func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteLabel(r.PathValue("key")); err != nil {
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
		if strings.HasPrefix(p, "/api/labels/") {
			p = "/api/labels/…"
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
