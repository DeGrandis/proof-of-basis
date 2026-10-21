package app

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// coinView is one unspent on-chain coin (UTXO) in a tracked wallet, with
// what an outside observer could link it to.
type coinView struct {
	Txid    string `json:"txid"`
	Vout    uint32 `json:"vout"`
	Wallet  string `json:"wallet"`
	Address string `json:"address"`
	Sats    int64  `json:"sats"`
	Height  int64  `json:"height"`
	Time    int64  `json:"t,omitempty"`
	// Cluster groups coins an observer can tie together: inputs spent in
	// the same transaction, and the change that came back from it.
	Cluster int `json:"cluster"`
	// Linked is set when the coin's history (within your wallets) reaches a
	// withdrawal from an exchange, which knows who you are.
	Linked   bool   `json:"linked"`
	LinkedTo string `json:"linkedTo,omitempty"`
}

// getCoins lists the unspent coins in your wallets with privacy linkage,
// from your scans and matching only.
func (s *Server) getCoins(w http.ResponseWriter, r *http.Request) {
	p, err := s.pipeline()
	if err != nil {
		fail(w, 500, err)
		return
	}
	wal, _, err := s.walletViews()
	if err != nil {
		fail(w, 500, err)
		return
	}
	type out struct {
		wallet, addr string
		sats         int64
	}
	type txv struct {
		id     string
		height int64
		t      int64
		ins    []string       // outpoints of ours it spends
		outs   map[string]out // outpoints of ours it creates
	}
	txs := map[string]*txv{}
	for _, wv := range wal {
		for _, t := range wv.txs {
			v := txs[t.Txid]
			if v == nil {
				v = &txv{id: t.Txid, height: t.Height, outs: map[string]out{}}
				if !t.Time.IsZero() {
					v.t = t.Time.Unix()
				}
				txs[t.Txid] = v
			}
			for _, in := range t.Inputs {
				if in.Ours {
					v.ins = append(v.ins, fmt.Sprintf("%s:%d", in.PrevTxid, in.PrevVout))
				}
			}
			for _, o := range t.Outputs {
				if o.Ours {
					v.outs[fmt.Sprintf("%s:%d", t.Txid, o.Vout)] = out{wv.name, o.Address, o.Sats}
				}
			}
		}
	}
	order := make([]*txv, 0, len(txs))
	for _, v := range txs {
		order = append(order, v)
	}
	sort.Slice(order, func(i, j int) bool {
		hi, hj := order[i].height, order[j].height
		if hi <= 0 {
			hi = 1 << 40
		}
		if hj <= 0 {
			hj = 1 << 40
		}
		if hi != hj {
			return hi < hj
		}
		return order[i].id < order[j].id
	})

	// Union-find over outpoints.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) { parent[find(a)] = find(b) }

	// Exchange withdrawals matched to a receipt in your wallets.
	linkedFrom := map[string]string{}
	for _, l := range p.tr.Links {
		if strings.HasPrefix(l.From, "ex:") && strings.HasPrefix(l.To, "tx:") {
			op := strings.TrimPrefix(l.To, "tx:")
			linkedFrom[op] = p.tr.Items[l.From].Account
		}
	}
	spent := map[string]bool{}
	linked := map[string]string{}
	for _, v := range order {
		var all []string
		all = append(all, v.ins...)
		for op := range v.outs {
			all = append(all, op)
		}
		for i := 1; i < len(all); i++ {
			union(all[0], all[i])
		}
		// Taint flows from any of our inputs to everything we got back.
		from := ""
		for _, in := range v.ins {
			spent[in] = true
			if linked[in] != "" && from == "" {
				from = linked[in]
			}
		}
		for op := range v.outs {
			if src, ok := linkedFrom[op]; ok {
				linked[op] = src
			} else if from != "" {
				linked[op] = from
			}
		}
	}
	coins := []coinView{}
	clusterID := map[string]int{}
	for _, v := range order {
		for op, o := range v.outs {
			if spent[op] {
				continue
			}
			root := find(op)
			if _, ok := clusterID[root]; !ok {
				clusterID[root] = len(clusterID) + 1
			}
			var vout uint32
			fmt.Sscanf(op[strings.LastIndexByte(op, ':')+1:], "%d", &vout)
			coins = append(coins, coinView{Txid: v.id, Vout: vout, Wallet: o.wallet, Address: o.addr, Sats: o.sats, Height: v.height, Time: v.t,
				Cluster: clusterID[root], Linked: linked[op] != "", LinkedTo: linked[op]})
		}
	}
	sort.Slice(coins, func(i, j int) bool {
		if coins[i].Cluster != coins[j].Cluster {
			return coins[i].Cluster < coins[j].Cluster
		}
		return coins[i].Sats > coins[j].Sats
	})
	writeJSON(w, map[string]any{"coins": coins, "clusters": len(clusterID)})
}
