// Package electrum is a minimal Electrum protocol client for talking to the
// user's own Fulcrum (or electrs) server. It never connects anywhere else.
package electrum

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Config describes how to reach the server.
type Config struct {
	Host string
	Port int
	TLS  bool
	// PinnedCertSHA256 is the hex SHA-256 of the server's leaf certificate.
	// Fulcrum usually runs with a self-signed cert, so it is pinned on first
	// use instead of being checked against public CAs.
	PinnedCertSHA256 string
	Timeout          time.Duration
}

type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	mu      sync.Mutex
	nextID  int
	timeout time.Duration
	// CertSHA256 is the fingerprint seen on connect (for pinning).
	CertSHA256 string
}

// Dial connects and performs the server.version handshake.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	d := net.Dialer{Timeout: 10 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: raw, timeout: cfg.Timeout}
	if cfg.TLS {
		var seen string
		tc := tls.Client(raw, &tls.Config{
			ServerName:         cfg.Host,
			InsecureSkipVerify: true, // verified below by pin
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return errors.New("no server certificate")
				}
				h := sha256.Sum256(rawCerts[0])
				seen = hex.EncodeToString(h[:])
				if cfg.PinnedCertSHA256 != "" && cfg.PinnedCertSHA256 != seen {
					return fmt.Errorf("server certificate changed (pinned %s, got %s)", cfg.PinnedCertSHA256, seen)
				}
				return nil
			},
		})
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		c.conn, c.CertSHA256 = tc, seen
	}
	c.r = bufio.NewReaderSize(c.conn, 1<<20)
	if _, err := c.Call("server.version", "proof-of-basis", "1.4"); err != nil {
		c.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	return c, nil
}

func (c *Client) Close() error { return c.conn.Close() }

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Call sends one request and waits for its response. Notifications that
// arrive in between (from subscriptions) are skipped.
func (c *Client) Call(method string, params ...any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	if params == nil {
		params = []any{}
	}
	b, _ := json.Marshal(request{"2.0", id, method, params})
	c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp response
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("bad response: %w", err)
		}
		if resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	}
}

type HistoryItem struct {
	TxHash string `json:"tx_hash"`
	Height int64  `json:"height"` // <=0 means unconfirmed
	Fee    int64  `json:"fee,omitempty"`
}

func (c *Client) History(scripthash string) ([]HistoryItem, error) {
	raw, err := c.Call("blockchain.scripthash.get_history", scripthash)
	if err != nil {
		return nil, err
	}
	var h []HistoryItem
	return h, json.Unmarshal(raw, &h)
}

type Balance struct {
	Confirmed   int64 `json:"confirmed"`
	Unconfirmed int64 `json:"unconfirmed"`
}

func (c *Client) Balance(scripthash string) (Balance, error) {
	raw, err := c.Call("blockchain.scripthash.get_balance", scripthash)
	if err != nil {
		return Balance{}, err
	}
	var b Balance
	return b, json.Unmarshal(raw, &b)
}

// RawTx returns the transaction hex.
func (c *Client) RawTx(txid string) (string, error) {
	raw, err := c.Call("blockchain.transaction.get", txid, false)
	if err != nil {
		return "", err
	}
	var s string
	return s, json.Unmarshal(raw, &s)
}

// Header returns the 80-byte block header hex at a height.
func (c *Client) Header(height int64) (string, error) {
	raw, err := c.Call("blockchain.block.header", height)
	if err != nil {
		return "", err
	}
	var s string
	return s, json.Unmarshal(raw, &s)
}

type Tip struct {
	Height int64  `json:"height"`
	Hex    string `json:"hex"`
}

func (c *Client) Tip() (Tip, error) {
	raw, err := c.Call("blockchain.headers.subscribe")
	if err != nil {
		return Tip{}, err
	}
	var t Tip
	return t, json.Unmarshal(raw, &t)
}
