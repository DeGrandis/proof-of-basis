package demo

import (
	"testing"

	"github.com/DeGrandis/proof-of-basis/internal/importers"
	"github.com/DeGrandis/proof-of-basis/internal/prices"
	"github.com/DeGrandis/proof-of-basis/internal/scan"
)

func TestStoryHasEveryKind(t *testing.T) {
	w := &world{rnd: newRand(1), price: prices.Bundled(), utxos: map[string][]utxo{}, views: map[string][]scan.Tx{},
		events: map[string][]importers.Event{}, bal: map[string]int64{}, height: 1}
	w.play()
	kinds := map[importers.Kind]int{}
	for _, evs := range w.events {
		for _, e := range evs {
			kinds[e.Kind]++
		}
	}
	t.Logf("%v; wallets hold %d / %d / %d / %d sats; exchanges %v", kinds, w.total(ledger), w.total(trezor), w.total(coldcard), w.total(phone), w.bal)
	for _, k := range []importers.Kind{importers.Buy, importers.Sell, importers.Withdrawal, importers.Deposit, importers.Income} {
		if kinds[k] == 0 {
			t.Errorf("no %s rows", k)
		}
	}
}
