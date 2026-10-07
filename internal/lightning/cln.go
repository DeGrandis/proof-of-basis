// Package lightning reads a Core Lightning node's bookkeeper over CLNRest
// with a rune. It only calls read methods; the rune should be restricted to
// them (see Settings). The rune is never logged or returned in errors.
package lightning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Client struct {
	base       string
	rune       string
	http       *http.Client
	CertSHA256 string // the server's leaf certificate, for pinning on first use
}

// Dial checks the URL and prepares a client. pinned is the hex SHA-256 of
// the server certificate seen before; empty trusts the first one seen.
func Dial(rawURL, rune, pinned string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("CLNRest address should look like https://192.168.1.10:3010")
	}
	if strings.TrimSpace(rune) == "" {
		return nil, errors.New("enter a rune for your node")
	}
	c := &Client{base: strings.TrimRight(u.String(), "/"), rune: strings.TrimSpace(rune)}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // CLNRest uses a self-signed certificate; pinned below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			sum := sha256.Sum256(raw[0])
			got := hex.EncodeToString(sum[:])
			if pinned != "" && !strings.EqualFold(got, pinned) {
				return fmt.Errorf("the node's TLS certificate changed (now %s…); clear the pin in Settings if you replaced it", got[:16])
			}
			c.CertSHA256 = got
			return nil
		},
	}}
	c.http = &http.Client{Transport: tr, Timeout: 60 * time.Second}
	return c, nil
}

// Call runs one RPC method through CLNRest.
func (c *Client) Call(ctx context.Context, method string, params map[string]any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Rune", c.rune)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is fine to show; never the rune, which is a header
		}
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 { // CLNRest answers 201 on success
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 || strings.Contains(strings.ToLower(e.Message), "rune") {
			return fmt.Errorf("%s: the node refused the rune (%s); it needs permission for bkpr-listincome, bkpr-listbalances and getinfo", method, e.Message)
		}
		return fmt.Errorf("%s: %s", method, e.Message)
	}
	return json.Unmarshal(b, out)
}

// Event is one bookkeeper income event, normalised.
type Event struct {
	UID         string `json:"uid"`
	Tag         string `json:"tag"`     // deposit withdrawal invoice invoice_fee routed onchain_fee ...
	Account     string `json:"account"` // "wallet" or a channel id
	CreditSats  int64  `json:"credit"`
	DebitSats   int64  `json:"debit"`
	Time        int64  `json:"t"`
	Description string `json:"description,omitempty"`
	Txid        string `json:"txid,omitempty"`
	Outpoint    string `json:"outpoint,omitempty"`
	PaymentID   string `json:"paymentId,omitempty"`
}

type rawIncome struct {
	Account     string `json:"account"`
	Tag         string `json:"tag"`
	Credit      int64  `json:"credit_msat"`
	Debit       int64  `json:"debit_msat"`
	Timestamp   int64  `json:"timestamp"`
	Description string `json:"description"`
	Outpoint    string `json:"outpoint"`
	Txid        string `json:"txid"`
	PaymentID   string `json:"payment_id"`
}

// Income returns the node's income events (payments, receipts, routing
// fees, on-chain deposits, withdrawals and fees), oldest first. Fees are
// consolidated per transaction (the default), which nets out the
// bookkeeper's internal balancing entries.
func (c *Client) Income(ctx context.Context) ([]Event, error) {
	var r struct {
		Events []rawIncome `json:"income_events"`
	}
	if err := c.Call(ctx, "bkpr-listincome", nil, &r); err != nil {
		return nil, err
	}
	return Normalise(r.Events), nil
}

// Normalise turns raw events into Events with stable ids, so syncing again
// never duplicates anything.
func Normalise(raw []rawIncome) []Event {
	seen := map[string]int{}
	out := make([]Event, 0, len(raw))
	for _, e := range raw {
		ev := Event{Tag: e.Tag, Account: e.Account, CreditSats: (e.Credit + 500) / 1000, DebitSats: (e.Debit + 500) / 1000,
			Time: e.Timestamp, Description: e.Description, Outpoint: e.Outpoint, Txid: e.Txid, PaymentID: e.PaymentID}
		if ev.Txid == "" && ev.Outpoint != "" {
			ev.Txid, _, _ = strings.Cut(ev.Outpoint, ":")
		}
		h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d|%d|%s|%s|%s", e.Account, e.Tag, e.Credit, e.Debit, e.Timestamp, e.Outpoint, e.Txid, e.PaymentID)))
		base := "cln:" + hex.EncodeToString(h[:10])
		seen[base]++
		ev.UID = base
		if seen[base] > 1 {
			ev.UID = fmt.Sprintf("%s#%d", base, seen[base])
		}
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// RawIncome is exported for tests and fixtures.
type RawIncome = rawIncome

// Channel is one account's balance as the bookkeeper sees it.
type Channel struct {
	Account string `json:"account"`
	Sats    int64  `json:"sats"`
	PeerID  string `json:"peerId,omitempty"`
	Closed  bool   `json:"closed,omitempty"`
}

// Balances returns every bookkeeper account's balance (the "wallet" account
// is the node's on-chain wallet; the rest are channels).
func (c *Client) Balances(ctx context.Context) ([]Channel, error) {
	var r struct {
		Accounts []struct {
			Account  string `json:"account"`
			PeerID   string `json:"peer_id"`
			Closed   bool   `json:"account_closed"`
			Resolved bool   `json:"account_resolved"`
			Balances []struct {
				Msat int64 `json:"balance_msat"`
			} `json:"balances"`
		} `json:"accounts"`
	}
	if err := c.Call(ctx, "bkpr-listbalances", nil, &r); err != nil {
		return nil, err
	}
	var out []Channel
	for _, a := range r.Accounts {
		var msat int64
		for _, b := range a.Balances {
			msat += b.Msat
		}
		if a.Account == "external" {
			continue
		}
		out = append(out, Channel{Account: a.Account, Sats: (msat + 500) / 1000, PeerID: a.PeerID, Closed: a.Closed})
	}
	return out, nil
}

// Info returns the node's alias and network, to confirm the connection.
func (c *Client) Info(ctx context.Context) (alias, network string, err error) {
	var r struct {
		Alias   string `json:"alias"`
		Network string `json:"network"`
	}
	err = c.Call(ctx, "getinfo", nil, &r)
	return r.Alias, r.Network, err
}
