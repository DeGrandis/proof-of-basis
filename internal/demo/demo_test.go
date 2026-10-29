package demo

import (
	"testing"

	"github.com/DeGrandis/proof-of-basis/internal/importers"
	"github.com/DeGrandis/proof-of-basis/internal/prices"
	"github.com/DeGrandis/proof-of-basis/internal/scan"
)

func TestStoryHasEveryKind(t *testing.T) {
	for _, div := range []int64{1, 10} {
		w := &world{rnd: newRand(1), price: prices.Bundled(), utxos: map[string][]utxo{}, views: map[string][]scan.Tx{},
			events: map[string][]importers.Event{}, bal: map[string]int64{}, height: 1, div: div}
		w.play()
		kinds := map[importers.Kind]int{}
		for _, evs := range w.events {
			for _, e := range evs {
				kinds[e.Kind]++
			}
		}
		held := w.total(ledger) + w.total(trezor) + w.total(coldcard) + w.total(phone)
		for _, b := range w.bal {
			held += b
		}
		t.Logf("1/%d: %v; %d sats held; wallets %d / %d / %d / %d; exchanges %v", div, kinds, held,
			w.total(ledger), w.total(trezor), w.total(coldcard), w.total(phone), w.bal)
		for _, k := range []importers.Kind{importers.Buy, importers.Sell, importers.Withdrawal, importers.Deposit, importers.Income} {
			if kinds[k] == 0 {
				t.Errorf("1/%d: no %s rows", div, k)
			}
		}
		// A tenth of the history is still hundreds of small buys, about a tenth of the coins.
		if div == 10 && (kinds[importers.Buy] < 300 || held < 8_000_000 || held > 20_000_000) {
			t.Errorf("1/10: %d buys, %d sats held", kinds[importers.Buy], held)
		}
	}
}
