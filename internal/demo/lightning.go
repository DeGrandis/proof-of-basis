package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lightning"
	"github.com/DeGrandis/proof-of-basis/internal/prices"
	"github.com/DeGrandis/proof-of-basis/internal/store"
	"github.com/DeGrandis/proof-of-basis/internal/trace"
)

// SeedLightning adds an invented Core Lightning node: an on-chain deposit
// into its wallet that opens a channel, then two years of payments sent and
// received and a little routing income. It looks synced; no node exists.
func SeedLightning(st *store.Store, seed int64) error {
	r := newRand(seed + 7)
	now := time.Now().UTC()
	t := now.AddDate(-2, 0, 0)
	const channel = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"
	var raw []lightning.RawIncome
	n := 0
	add := func(tag, acct string, credit, debit int64, desc, txid, payment string) {
		n++
		raw = append(raw, lightning.RawIncome{Account: acct, Tag: tag, Credit: credit * 1000, Debit: debit * 1000,
			Timestamp: t.Unix() + int64(n), Description: desc, Txid: txid, PaymentID: payment})
	}
	fake := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

	add("deposit", "wallet", 3_000_000, 0, "", fake("demo-ln-deposit"), "")
	t = t.Add(2 * time.Hour)
	add("onchain_fee", "wallet", 0, 2_480, "", fake("demo-ln-open"), "")
	paid := []string{"Coffee", "VPN subscription", "Podcast boost", "Sticker shop", "Hosting", "Book", "Lunch", "Donation to a dev fund"}
	got := []string{"Tip received", "Invoice paid by a client", "Refund from a shop", "Zap"}
	var spent int64
	for t.Before(now.AddDate(0, 0, -3)) {
		t = t.Add(time.Duration(3+r.Intn(9)) * 24 * time.Hour)
		switch x := r.Intn(10); {
		case x < 6 && spent < 2_200_000:
			sats := int64(2_000 + r.Intn(45_000))
			id := fake(fmt.Sprintf("demo-ln-pay-%d", n))
			add("invoice", channel, 0, sats, paid[r.Intn(len(paid))], "", id)
			add("invoice_fee", channel, 0, 1+int64(r.Intn(40)), "", "", id)
			spent += sats
		case x < 8:
			add("invoice", channel, int64(5_000+r.Intn(60_000)), 0, got[r.Intn(len(got))], "", fake(fmt.Sprintf("demo-ln-recv-%d", n)))
		default:
			add("routed", channel, int64(1+r.Intn(60)), 0, "", "", "")
		}
	}
	evs := lightning.Normalise(raw)
	if _, err := st.PutLightning(evs); err != nil {
		return err
	}
	// The deposit came from a wallet that isn't added, at the day's price.
	// Left unlabeled, the node would count its payments but not its
	// funding, and show a negative balance.
	for _, e := range evs {
		if e.Tag == "deposit" {
			p := (&world{price: prices.Bundled()}).priceOn(time.Unix(e.Time, 0).UTC())
			if err := st.SetLabel(trace.Label{Key: "ln:" + e.UID, Category: trace.Untracked, FiatCents: p * e.CreditSats / 100_000_000,
				Note: "Funded the node from a hardware wallet not added here"}); err != nil {
				return err
			}
		}
	}
	var onchain, inChannel int64
	for _, e := range evs {
		if e.Account == "wallet" {
			onchain += e.CreditSats - e.DebitSats
		} else {
			inChannel += e.CreditSats - e.DebitSats
		}
	}
	// The deposit opened the channel: the node's wallet keeps the change.
	opened := onchain * 9 / 10
	chans, _ := json.Marshal([]lightning.Channel{{Account: "wallet", Sats: onchain - opened},
		{Account: channel, Sats: opened + inChannel, PeerID: "03" + fake("demo-ln-peer")[:64]}})
	for k, v := range map[string]string{"clnName": "Lightning node", "clnAlias": "demo-node",
		"clnChannels": string(chans), "clnSynced": now.Format(time.RFC3339)} {
		if err := st.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}
