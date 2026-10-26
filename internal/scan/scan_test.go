package scan

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/DeGrandis/proof-of-basis/internal/electrum"
	"github.com/DeGrandis/proof-of-basis/internal/wallet"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// BIP84 test-vector zpub; all transactions below are invented.
const testZpub = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"

type fake struct {
	hist map[string][]electrum.HistoryItem
	txs  map[string]string
}

func (f *fake) History(sh string) ([]electrum.HistoryItem, error) { return f.hist[sh], nil }
func (f *fake) RawTx(id string) (string, error)                   { return f.txs[id], nil }
func (f *fake) Header(h int64) (string, error) {
	b := make([]byte, 80)
	binary.LittleEndian.PutUint32(b[68:], uint32(1_700_000_000+h*600))
	return hex.EncodeToString(b), nil
}

func spk(t *testing.T, a string) []byte {
	addr, err := btcutil.DecodeAddress(a, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := txscript.PayToAddrScript(addr)
	return s
}

func TestScan(t *testing.T) {
	acct, err := wallet.ParseExtendedKey(testZpub, "", false)
	if err != nil {
		t.Fatal(err)
	}
	recv, _ := acct.Derive(0, 0)
	change, _ := acct.Derive(1, 0)
	external := "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq" // BIP173 example address

	tx1 := wire.NewMsgTx(2)
	tx1.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{1}, 0), nil, nil))
	tx1.AddTxOut(wire.NewTxOut(100_000, recv.ScriptPubKey))
	h1 := tx1.TxHash()

	tx2 := wire.NewMsgTx(2)
	tx2.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&h1, 0), nil, nil))
	tx2.AddTxOut(wire.NewTxOut(60_000, spk(t, external)))
	tx2.AddTxOut(wire.NewTxOut(39_000, change.ScriptPubKey))
	h2 := tx2.TxHash()

	ser := func(m *wire.MsgTx) string {
		var b bytes.Buffer
		m.Serialize(&b)
		return hex.EncodeToString(b.Bytes())
	}
	f := &fake{
		hist: map[string][]electrum.HistoryItem{
			recv.ScriptHash:   {{TxHash: h1.String(), Height: 10}, {TxHash: h2.String(), Height: 12}},
			change.ScriptHash: {{TxHash: h2.String(), Height: 12}},
		},
		txs: map[string]string{h1.String(): ser(tx1), h2.String(): ser(tx2)},
	}
	res, err := Wallet(f, acct, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Balance != 39_000 {
		t.Errorf("balance %d", res.Balance)
	}
	if len(res.Txs) != 2 || res.Txs[0].Txid != h1.String() {
		t.Fatalf("txs %+v", res.Txs)
	}
	if got := res.Txs[1]; got.Net() != -61_000 || got.FeeSats != 1_000 {
		t.Errorf("spend net %d fee %d", got.Net(), got.FeeSats)
	}
	if res.Txs[0].Time.Unix() != 1_700_006_000 {
		t.Errorf("time %v", res.Txs[0].Time)
	}
	if len(res.Addresses) != 12 { // 1 used + 5 gap on each chain
		t.Errorf("derived %d addresses", len(res.Addresses))
	}
}
