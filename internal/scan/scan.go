// Package scan discovers a wallet's addresses (gap-limit scan) and
// summarises each transaction that touches it.
package scan

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/electrum"
	"github.com/DeGrandis/proof-of-basis/internal/wallet"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Source is the subset of the Electrum client the scanner needs.
type Source interface {
	History(scripthash string) ([]electrum.HistoryItem, error)
	RawTx(txid string) (string, error)
	Header(height int64) (string, error)
}

type Output struct {
	Vout    uint32
	Address string // empty for non-standard scripts
	Sats    int64
	Ours    bool
}

type Input struct {
	PrevTxid string
	PrevVout uint32
	Sats     int64 // known only when the prevout is ours
	Ours     bool
}

// Tx is one transaction as seen from a single wallet.
type Tx struct {
	Txid    string
	Height  int64
	Time    time.Time // block time; zero if unconfirmed
	Inputs  []Input
	Outputs []Output
	// SentSats is the value of our inputs; RecvSats the value of outputs to us.
	SentSats, RecvSats int64
	// FeeSats is set when every input is ours (we paid the fee).
	FeeSats int64
	RawHex  string
}

// Net is the change in the wallet's balance.
func (t Tx) Net() int64 { return t.RecvSats - t.SentSats }

type Result struct {
	Addresses []wallet.Address
	Used      map[string]bool // address -> has history
	Txs       []Tx
	Balance   int64
}

// Wallet scans receive and change chains until gap consecutive unused
// addresses, then fetches and summarises every transaction.
func Wallet(src Source, acct wallet.Deriver, gap int) (*Result, error) {
	if gap <= 0 {
		gap = 20
	}
	res := &Result{Used: map[string]bool{}}
	ours := map[string]bool{} // address strings
	heights := map[string]int64{}
	for chain := uint32(0); chain < acct.Chains(); chain++ {
		unused := 0
		for i := uint32(0); unused < gap; i++ {
			a, err := acct.Derive(chain, i)
			if err != nil {
				return nil, err
			}
			res.Addresses = append(res.Addresses, a)
			ours[a.Address] = true
			h, err := src.History(a.ScriptHash)
			if err != nil {
				return nil, fmt.Errorf("history %s: %w", a.Address, err)
			}
			if len(h) == 0 {
				unused++
				continue
			}
			unused = 0
			res.Used[a.Address] = true
			for _, it := range h {
				heights[it.TxHash] = it.Height
			}
		}
	}

	msgs := map[string]*wire.MsgTx{}
	raws := map[string]string{}
	for txid := range heights {
		raw, err := src.RawTx(txid)
		if err != nil {
			return nil, fmt.Errorf("tx %s: %w", txid, err)
		}
		b, err := hex.DecodeString(raw)
		if err != nil {
			return nil, err
		}
		var m wire.MsgTx
		if err := m.Deserialize(bytes.NewReader(b)); err != nil {
			return nil, fmt.Errorf("decode %s: %w", txid, err)
		}
		msgs[txid], raws[txid] = &m, raw
	}

	// Index our outputs so inputs spending them can be recognised.
	type outpoint struct {
		txid string
		vout uint32
	}
	ourOuts := map[outpoint]int64{}
	for txid, m := range msgs {
		for i, o := range m.TxOut {
			if ours[addrOf(o.PkScript, acct.Network())] {
				ourOuts[outpoint{txid, uint32(i)}] = o.Value
			}
		}
	}

	times := map[int64]time.Time{}
	for txid, m := range msgs {
		t := Tx{Txid: txid, Height: heights[txid], RawHex: raws[txid]}
		if t.Height > 0 {
			if _, ok := times[t.Height]; !ok {
				ts, err := blockTime(src, t.Height)
				if err != nil {
					return nil, err
				}
				times[t.Height] = ts
			}
			t.Time = times[t.Height]
		}
		allOurs := len(m.TxIn) > 0
		for _, in := range m.TxIn {
			op := outpoint{in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index}
			v, mine := ourOuts[op]
			t.Inputs = append(t.Inputs, Input{op.txid, op.vout, v, mine})
			if mine {
				t.SentSats += v
			} else {
				allOurs = false
			}
		}
		var outTotal int64
		for i, o := range m.TxOut {
			a := addrOf(o.PkScript, acct.Network())
			mine := ours[a]
			t.Outputs = append(t.Outputs, Output{uint32(i), a, o.Value, mine})
			outTotal += o.Value
			if mine {
				t.RecvSats += o.Value
			}
		}
		if allOurs {
			t.FeeSats = t.SentSats - outTotal
		}
		res.Balance += t.Net()
		res.Txs = append(res.Txs, t)
	}
	sort.Slice(res.Txs, func(i, j int) bool {
		a, b := res.Txs[i], res.Txs[j]
		if (a.Height <= 0) != (b.Height <= 0) {
			return b.Height <= 0 // confirmed first
		}
		if a.Height != b.Height {
			return a.Height < b.Height
		}
		return a.Txid < b.Txid
	})
	return res, nil
}

func addrOf(pk []byte, params *chaincfg.Params) string {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(pk, params)
	if err != nil || len(addrs) != 1 {
		return ""
	}
	return addrs[0].EncodeAddress()
}

func blockTime(src Source, height int64) (time.Time, error) {
	h, err := src.Header(height)
	if err != nil {
		return time.Time{}, fmt.Errorf("header %d: %w", height, err)
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 80 {
		return time.Time{}, fmt.Errorf("bad header at %d", height)
	}
	return time.Unix(int64(binary.LittleEndian.Uint32(b[68:72])), 0).UTC(), nil
}
