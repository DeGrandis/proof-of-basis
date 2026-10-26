package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"btctax/internal/scan"
	"btctax/internal/wallet"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Lookups answer from the user's own data first and then ask only the
// user's own Fulcrum server. Nothing here contacts anyone else.

var txidRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

type txIn struct {
	PrevTxid string `json:"prevTxid"`
	PrevVout uint32 `json:"prevVout"`
	Address  string `json:"address,omitempty"`
	Sats     int64  `json:"sats,omitempty"` // 0 when unknown
	Wallet   string `json:"wallet,omitempty"`
}

type txOut struct {
	Vout    uint32 `json:"vout"`
	Address string `json:"address,omitempty"`
	Sats    int64  `json:"sats"`
	Wallet  string `json:"wallet,omitempty"`
}

type exRef struct {
	Key     string `json:"key"`
	Account string `json:"account"`
	Kind    string `json:"kind"`
	Sats    int64  `json:"sats"`
	Time    int64  `json:"t"`
}

type txView struct {
	Txid     string  `json:"txid"`
	Height   int64   `json:"height"`
	Time     int64   `json:"t,omitempty"`
	FeeSats  int64   `json:"feeSats,omitempty"`
	Inputs   []txIn  `json:"inputs"`
	Outputs  []txOut `json:"outputs"`
	Exchange []exRef `json:"exchange"`
	Source   string  `json:"source"` // "wallets", "fulcrum" or "records"
	Note     string  `json:"note,omitempty"`
}

func (s *Server) params() *chaincfg.Params {
	if s.st.Setting("regtest", "false") == "true" {
		return &chaincfg.RegressionNetParams
	}
	return &chaincfg.MainNetParams
}

// localTx merges every tracked wallet's view of a transaction.
func localTx(wal []walletView, txid string) *txView {
	var v *txView
	for _, w := range wal {
		for _, t := range w.txs {
			if t.Txid != txid {
				continue
			}
			if v == nil {
				v = &txView{Txid: txid, Height: t.Height, Source: "wallets"}
				if !t.Time.IsZero() {
					v.Time = t.Time.Unix()
				}
				for _, in := range t.Inputs {
					v.Inputs = append(v.Inputs, txIn{PrevTxid: in.PrevTxid, PrevVout: in.PrevVout})
				}
				for _, o := range t.Outputs {
					v.Outputs = append(v.Outputs, txOut{Vout: o.Vout, Address: o.Address, Sats: o.Sats})
				}
			}
			for i, in := range t.Inputs {
				if in.Ours && i < len(v.Inputs) {
					v.Inputs[i].Sats, v.Inputs[i].Wallet = in.Sats, w.name
				}
			}
			for i, o := range t.Outputs {
				if o.Ours && i < len(v.Outputs) {
					v.Outputs[i].Wallet = w.name
				}
			}
			if t.FeeSats > 0 {
				v.FeeSats = t.FeeSats
			}
		}
	}
	if v != nil && v.FeeSats == 0 {
		var in, out int64
		known := true
		for _, x := range v.Inputs {
			if x.Sats == 0 {
				known = false
			}
			in += x.Sats
		}
		for _, x := range v.Outputs {
			out += x.Sats
		}
		if known && in >= out {
			v.FeeSats = in - out
		}
	}
	return v
}

type walletView struct {
	name string
	txs  []scan.Tx
}

func (s *Server) walletViews() ([]walletView, map[string]string, error) {
	ws, err := s.st.WalletTxs()
	if err != nil {
		return nil, nil, err
	}
	owner := map[string]string{} // address -> wallet, for every address we've seen as ours
	out := make([]walletView, 0, len(ws))
	for _, w := range ws {
		out = append(out, walletView{w.Account, w.Txs})
		for _, t := range w.Txs {
			for _, o := range t.Outputs {
				if o.Ours && o.Address != "" {
					owner[o.Address] = w.Account
				}
			}
		}
	}
	return out, owner, nil
}

func (s *Server) exchangeRefs(match func(txid, addr string) bool) ([]exRef, error) {
	ex, err := s.st.ExchangeEvents()
	if err != nil {
		return nil, err
	}
	out := []exRef{}
	for _, e := range ex {
		if match(e.Txid, e.Address) {
			out = append(out, exRef{"ex:" + e.ID, e.Account, string(e.Kind), e.Sats, e.Time.Unix()})
		}
	}
	return out, nil
}

func (s *Server) getTx(w http.ResponseWriter, r *http.Request) {
	txid := strings.ToLower(r.PathValue("txid"))
	if !txidRe.MatchString(txid) {
		fail(w, 400, errors.New("a transaction id is 64 hex characters"))
		return
	}
	wal, owner, err := s.walletViews()
	if err != nil {
		fail(w, 500, err)
		return
	}
	exs, err := s.exchangeRefs(func(t, _ string) bool { return strings.EqualFold(t, txid) })
	if err != nil {
		fail(w, 500, err)
		return
	}
	v := localTx(wal, txid)
	if v != nil {
		// Inputs spending coins we've seen get their address from the
		// output they spend.
		prev := map[string]txOut{}
		for _, wv := range wal {
			for _, t := range wv.txs {
				for _, o := range t.Outputs {
					prev[fmt.Sprintf("%s:%d", t.Txid, o.Vout)] = txOut{Address: o.Address, Sats: o.Sats}
				}
			}
		}
		for i, in := range v.Inputs {
			if p, ok := prev[fmt.Sprintf("%s:%d", in.PrevTxid, in.PrevVout)]; ok {
				v.Inputs[i].Address = p.Address
				if v.Inputs[i].Sats == 0 {
					v.Inputs[i].Sats = p.Sats
				}
			}
		}
	}
	if v == nil {
		v, err = s.fulcrumTx(r.Context(), txid, owner)
		if err != nil {
			if len(exs) == 0 {
				fail(w, 404, err)
				return
			}
			v = &txView{Txid: txid, Source: "records", Note: err.Error()}
		}
	}
	if v.Inputs == nil {
		v.Inputs = []txIn{}
	}
	if v.Outputs == nil {
		v.Outputs = []txOut{}
	}
	v.Exchange = exs
	writeJSON(w, v)
}

// fulcrumTx decodes a transaction fetched from the user's Fulcrum, with
// input amounts from the transactions they spend (up to 25 of them).
func (s *Server) fulcrumTx(ctx context.Context, txid string, owner map[string]string) (*txView, error) {
	if s.demo != nil {
		return nil, errors.New("not in the demo's records")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := s.dial(ctx)
	if err != nil {
		return nil, errors.New("not in your records, and Fulcrum isn't reachable: " + err.Error())
	}
	defer c.Close()
	src := cachedSource{c, s.st}
	m, err := decodeTx(src, txid)
	if err != nil {
		return nil, errors.New("your Fulcrum server doesn't know this transaction")
	}
	p := s.params()
	v := &txView{Txid: txid, Source: "fulcrum"}
	var outTotal int64
	for i, o := range m.TxOut {
		a := addrOf(o.PkScript, p)
		v.Outputs = append(v.Outputs, txOut{Vout: uint32(i), Address: a, Sats: o.Value, Wallet: owner[a]})
		outTotal += o.Value
	}
	var inTotal int64
	known := true
	for i, in := range m.TxIn {
		x := txIn{PrevTxid: in.PreviousOutPoint.Hash.String(), PrevVout: in.PreviousOutPoint.Index}
		if i < 25 && !isCoinbase(in) {
			if prev, err := decodeTx(src, x.PrevTxid); err == nil && int(x.PrevVout) < len(prev.TxOut) {
				po := prev.TxOut[x.PrevVout]
				x.Sats, x.Address = po.Value, addrOf(po.PkScript, p)
				x.Wallet = owner[x.Address]
			}
		}
		if x.Sats == 0 {
			known = false
		}
		inTotal += x.Sats
		v.Inputs = append(v.Inputs, x)
	}
	if known && inTotal >= outTotal {
		v.FeeSats = inTotal - outTotal
	}
	// The height comes from the history of one of its outputs.
	if len(m.TxOut) > 0 {
		if h, err := c.History(wallet.ScriptHash(m.TxOut[0].PkScript)); err == nil {
			for _, it := range h {
				if it.TxHash == txid {
					v.Height = it.Height
				}
			}
		}
	}
	if v.Height > 0 {
		if hdr, err := src.Header(v.Height); err == nil {
			if b, err := hex.DecodeString(hdr); err == nil && len(b) == 80 {
				v.Time = int64(uint32(b[68]) | uint32(b[69])<<8 | uint32(b[70])<<16 | uint32(b[71])<<24)
			}
		}
	}
	return v, nil
}

func isCoinbase(in *wire.TxIn) bool {
	return in.PreviousOutPoint.Index == 0xffffffff && in.PreviousOutPoint.Hash == [32]byte{}
}

func decodeTx(src cachedSource, txid string) (*wire.MsgTx, error) {
	raw, err := src.RawTx(txid)
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var m wire.MsgTx
	if err := m.Deserialize(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	return &m, nil
}

func addrOf(pk []byte, p *chaincfg.Params) string {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(pk, p)
	if err != nil || len(addrs) != 1 {
		return ""
	}
	return addrs[0].EncodeAddress()
}

type addrTx struct {
	Txid   string `json:"txid"`
	Height int64  `json:"height"`
	Local  bool   `json:"local"`
}

func (s *Server) getAddress(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimSpace(r.PathValue("addr"))
	if addr == "" || len(addr) > 120 {
		fail(w, 400, errors.New("enter a bitcoin address"))
		return
	}
	wal, owner, err := s.walletViews()
	if err != nil {
		fail(w, 500, err)
		return
	}
	seen := map[string]*addrTx{}
	for _, wv := range wal {
		for _, t := range wv.txs {
			for _, o := range t.Outputs {
				if o.Address == addr && seen[t.Txid] == nil {
					seen[t.Txid] = &addrTx{t.Txid, t.Height, true}
				}
			}
		}
	}
	exs, err := s.exchangeRefs(func(_, a string) bool { return a == addr })
	if err != nil {
		fail(w, 500, err)
		return
	}
	out := map[string]any{"address": addr, "wallet": owner[addr], "exchange": exs}
	// Then the user's own Fulcrum, which also knows spends from the address.
	if a, err := btcutil.DecodeAddress(addr, s.params()); err != nil {
		out["note"] = "not a valid address for this network"
	} else if spk, err := txscript.PayToAddrScript(a); err == nil && s.demo == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if c, err := s.dial(ctx); err != nil {
			out["note"] = "only your records were searched: " + err.Error()
		} else {
			if h, err := c.History(wallet.ScriptHash(spk)); err == nil {
				for _, it := range h {
					if seen[it.TxHash] == nil {
						seen[it.TxHash] = &addrTx{it.TxHash, it.Height, false}
					} else {
						seen[it.TxHash].Height = it.Height
					}
				}
				out["fulcrum"] = true
			} else {
				out["note"] = "Fulcrum lookup failed: " + err.Error()
			}
			c.Close()
		}
	}
	txs := make([]addrTx, 0, len(seen))
	for _, t := range seen {
		txs = append(txs, *t)
	}
	sort.Slice(txs, func(i, j int) bool {
		hi, hj := txs[i].Height, txs[j].Height
		if hi <= 0 {
			hi = 1 << 40
		}
		if hj <= 0 {
			hj = 1 << 40
		}
		return hi > hj
	})
	out["txs"] = txs
	writeJSON(w, out)
}

// Recheck rescans every wallet in the background, keeping all data and
// labels; matching and tax are recomputed on the next report as always.
type recheckState struct {
	Running  bool     `json:"running"`
	Total    int      `json:"total"`
	Done     int      `json:"done"`
	Current  string   `json:"current,omitempty"`
	Errors   []string `json:"errors"`
	Finished string   `json:"finished,omitempty"`
}

type rechecker struct {
	mu sync.Mutex
	st recheckState
}

func (s *Server) recheckStatus(w http.ResponseWriter, r *http.Request) {
	s.recheck.mu.Lock()
	defer s.recheck.mu.Unlock()
	st := s.recheck.st
	if st.Errors == nil {
		st.Errors = []string{}
	}
	writeJSON(w, st)
}

func (s *Server) startRecheck(w http.ResponseWriter, r *http.Request) {
	accts, err := s.st.Accounts()
	if err != nil {
		fail(w, 500, err)
		return
	}
	var ids []int64
	names := map[int64]string{}
	for _, a := range accts {
		if a.Kind == "wallet" {
			ids = append(ids, a.ID)
			names[a.ID] = a.Name
		}
	}
	s.recheck.mu.Lock()
	if s.recheck.st.Running {
		s.recheck.mu.Unlock()
		s.recheckStatus(w, r)
		return
	}
	s.recheck.st = recheckState{Running: true, Total: len(ids), Errors: []string{}}
	s.recheck.mu.Unlock()
	go func() {
		if s.st.Setting("clnUrl", "") != "" {
			s.recheck.mu.Lock()
			s.recheck.st.Current = s.lnAccount()
			s.recheck.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if _, err := s.lightningSync(ctx); err != nil {
				s.recheck.mu.Lock()
				s.recheck.st.Errors = append(s.recheck.st.Errors, s.lnAccount()+": "+err.Error())
				s.recheck.mu.Unlock()
			}
			cancel()
		}
		for _, id := range ids {
			s.recheck.mu.Lock()
			s.recheck.st.Current = names[id]
			s.recheck.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			_, err := s.rescan(ctx, id)
			cancel()
			s.recheck.mu.Lock()
			s.recheck.st.Done++
			if err != nil {
				s.recheck.st.Errors = append(s.recheck.st.Errors, names[id]+": "+err.Error())
			}
			s.recheck.mu.Unlock()
		}
		s.recheck.mu.Lock()
		s.recheck.st.Running, s.recheck.st.Current = false, ""
		s.recheck.st.Finished = time.Now().UTC().Format(time.RFC3339)
		s.recheck.mu.Unlock()
	}()
	s.recheckStatus(w, r)
}

// rescan scans one wallet with the user's Fulcrum and saves the result.
func (s *Server) rescan(ctx context.Context, id int64) (*scan.Result, error) {
	a, err := s.st.Account(id)
	if err != nil || a.Kind != "wallet" {
		return nil, errors.New("no such wallet")
	}
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	acct, err := s.parseWallet(walletIn{Key: a.XPub, Script: a.Script})
	if err != nil {
		return nil, err
	}
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := scan.Wallet(cachedSource{c, s.st}, acct, a.Gap)
	if err != nil {
		return nil, err
	}
	return res, s.st.SaveScan(id, res)
}
