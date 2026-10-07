// Package store persists everything in one SQLite file.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"btctax/internal/importers"
	"btctax/internal/scan"
	"btctax/internal/trace"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS accounts (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('exchange','wallet')),
  name TEXT NOT NULL UNIQUE,
  handler TEXT NOT NULL DEFAULT '',
  xpub TEXT NOT NULL DEFAULT '',
  script TEXT NOT NULL DEFAULT '',
  gap INTEGER NOT NULL DEFAULT 20,
  balance INTEGER NOT NULL DEFAULT 0,
  scanned_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS imports (
  id INTEGER PRIMARY KEY,
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  handler TEXT NOT NULL,
  filename TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  imported_at TEXT NOT NULL,
  rows INTEGER NOT NULL,
  warnings TEXT NOT NULL DEFAULT '[]',
  UNIQUE (account_id, sha256)
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  import_id INTEGER NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  data TEXT NOT NULL,
  uid TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS wallet_txs (
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  txid TEXT NOT NULL,
  data TEXT NOT NULL,
  PRIMARY KEY (account_id, txid)
);
CREATE TABLE IF NOT EXISTS raw_txs (txid TEXT PRIMARY KEY, hex TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS headers (height INTEGER PRIMARY KEY, hex TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS labels (key TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS prices (day TEXT PRIMARY KEY, cents INTEGER NOT NULL, source TEXT NOT NULL);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Added after the first version; fails harmlessly when present.
	db.Exec(`ALTER TABLE events ADD COLUMN uid TEXT NOT NULL DEFAULT ''`)
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS events_uid ON events(account_id, uid) WHERE uid != ''`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Settings

func (s *Store) Setting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Accounts

type Account struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Handler   string `json:"handler,omitempty"`
	XPub      string `json:"-"`
	Script    string `json:"script,omitempty"`
	Gap       int    `json:"gap,omitempty"`
	Balance   int64  `json:"balance"`
	ScannedAt string `json:"scannedAt,omitempty"`
}

func (s *Store) Accounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT id,kind,name,handler,xpub,script,gap,balance,scanned_at FROM accounts ORDER BY kind DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Kind, &a.Name, &a.Handler, &a.XPub, &a.Script, &a.Gap, &a.Balance, &a.ScannedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Account(id int64) (Account, error) {
	all, err := s.Accounts()
	if err != nil {
		return Account{}, err
	}
	for _, a := range all {
		if a.ID == id {
			return a, nil
		}
	}
	return Account{}, sql.ErrNoRows
}

func (s *Store) AddWallet(name, xpub, script string, gap int) (int64, error) {
	r, err := s.db.Exec(`INSERT INTO accounts(kind,name,xpub,script,gap) VALUES('wallet',?,?,?,?)`, name, xpub, script, gap)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// ExchangeAccount returns the id of the named exchange account, creating it.
func (s *Store) ExchangeAccount(name, handler string) (int64, error) {
	var id int64
	var kind string
	err := s.db.QueryRow(`SELECT id, kind FROM accounts WHERE name=?`, name).Scan(&id, &kind)
	if err == nil {
		if kind != "exchange" {
			return 0, fmt.Errorf("%q is a wallet, not an exchange account", name)
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	r, err := s.db.Exec(`INSERT INTO accounts(kind,name,handler) VALUES('exchange',?,?)`, name, handler)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) DeleteAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}

// Imports

type Import struct {
	ID         int64               `json:"id"`
	AccountID  int64               `json:"accountId"`
	Account    string              `json:"account"`
	Handler    string              `json:"handler"`
	Filename   string              `json:"filename"`
	ImportedAt string              `json:"importedAt"`
	Rows       int                 `json:"rows"`
	Warnings   []importers.Warning `json:"warnings"`
}

var ErrDuplicate = errors.New("this file was already imported for this account")

// AddImport stores a parsed file. Rows already imported for this account
// (same UID, e.g. from an overlapping export) are skipped and counted.
func (s *Store) AddImport(accountID int64, handler, filename, sha string, evs []importers.Event, warns []importers.Warning) (id int64, skipped int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var exists int
	tx.QueryRow(`SELECT COUNT(*) FROM imports WHERE account_id=? AND sha256=?`, accountID, sha).Scan(&exists)
	if exists > 0 {
		return 0, 0, ErrDuplicate
	}
	wj, _ := json.Marshal(warns)
	r, err := tx.Exec(`INSERT INTO imports(account_id,handler,filename,sha256,imported_at,rows,warnings) VALUES(?,?,?,?,?,?,?)`,
		accountID, handler, filename, sha, time.Now().UTC().Format(time.RFC3339), len(evs), string(wj))
	if err != nil {
		return 0, 0, err
	}
	id, _ = r.LastInsertId()
	for _, e := range evs {
		b, _ := json.Marshal(e)
		res, err := tx.Exec(`INSERT OR IGNORE INTO events(import_id,account_id,data,uid) VALUES(?,?,?,?)`, id, accountID, string(b), e.UID)
		if err != nil {
			return 0, 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			skipped++
		}
	}
	if _, err := tx.Exec(`UPDATE imports SET rows=? WHERE id=?`, len(evs)-skipped, id); err != nil {
		return 0, 0, err
	}
	return id, skipped, tx.Commit()
}

func (s *Store) Imports() ([]Import, error) {
	rows, err := s.db.Query(`SELECT i.id,i.account_id,a.name,i.handler,i.filename,i.imported_at,i.rows,i.warnings
		FROM imports i JOIN accounts a ON a.id=i.account_id ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Import
	for rows.Next() {
		var im Import
		var wj string
		if err := rows.Scan(&im.ID, &im.AccountID, &im.Account, &im.Handler, &im.Filename, &im.ImportedAt, &im.Rows, &wj); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(wj), &im.Warnings)
		out = append(out, im)
	}
	return out, rows.Err()
}

func (s *Store) DeleteImport(id int64) error {
	_, err := s.db.Exec(`DELETE FROM imports WHERE id=?`, id)
	return err
}

// ExchangeEvents returns all imported events keyed for tracing.
func (s *Store) ExchangeEvents() ([]trace.ExEvent, error) {
	rows, err := s.db.Query(`SELECT e.id, a.name, e.data FROM events e JOIN accounts a ON a.id=e.account_id ORDER BY e.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []trace.ExEvent
	for rows.Next() {
		var id int64
		var acct, data string
		if err := rows.Scan(&id, &acct, &data); err != nil {
			return nil, err
		}
		var e importers.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, err
		}
		e.Raw = nil
		out = append(out, trace.ExEvent{ID: fmt.Sprint(id), Account: acct, Event: e})
	}
	return out, rows.Err()
}

// Wallet transactions

func (s *Store) SaveScan(accountID int64, res *scan.Result) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM wallet_txs WHERE account_id=?`, accountID); err != nil {
		return err
	}
	for _, t := range res.Txs {
		raw := t.RawHex
		t.RawHex = ""
		b, _ := json.Marshal(t)
		if _, err := tx.Exec(`INSERT INTO wallet_txs(account_id,txid,data) VALUES(?,?,?)`, accountID, t.Txid, string(b)); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO raw_txs(txid,hex) VALUES(?,?)`, t.Txid, raw); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE accounts SET balance=?, scanned_at=? WHERE id=?`, res.Balance, time.Now().UTC().Format(time.RFC3339), accountID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) WalletTxs() ([]trace.WalletTxs, error) {
	rows, err := s.db.Query(`SELECT a.name, w.data FROM wallet_txs w JOIN accounts a ON a.id=w.account_id ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]*trace.WalletTxs{}
	var order []string
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			return nil, err
		}
		var t scan.Tx
		if err := json.Unmarshal([]byte(data), &t); err != nil {
			return nil, err
		}
		if by[name] == nil {
			by[name] = &trace.WalletTxs{Account: name}
			order = append(order, name)
		}
		by[name].Txs = append(by[name].Txs, t)
	}
	out := make([]trace.WalletTxs, 0, len(order))
	for _, n := range order {
		out = append(out, *by[n])
	}
	return out, rows.Err()
}

// Cache for raw transactions and headers, so rescans are cheap.

func (s *Store) CachedTx(txid string) (string, bool) {
	var h string
	err := s.db.QueryRow(`SELECT hex FROM raw_txs WHERE txid=?`, txid).Scan(&h)
	return h, err == nil
}

func (s *Store) CachedHeader(height int64) (string, bool) {
	var h string
	err := s.db.QueryRow(`SELECT hex FROM headers WHERE height=?`, height).Scan(&h)
	return h, err == nil
}

func (s *Store) PutHeader(height int64, hex string) {
	s.db.Exec(`INSERT OR REPLACE INTO headers(height,hex) VALUES(?,?)`, height, hex)
}

// Labels

func (s *Store) Labels() (map[string]trace.Label, error) {
	rows, err := s.db.Query(`SELECT data FROM labels`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]trace.Label{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var l trace.Label
		if err := json.Unmarshal([]byte(data), &l); err != nil {
			return nil, err
		}
		out[l.Key] = l
	}
	return out, rows.Err()
}

func (s *Store) SetLabel(l trace.Label) error {
	b, _ := json.Marshal(l)
	_, err := s.db.Exec(`INSERT INTO labels(key,data) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, l.Key, string(b))
	return err
}

func (s *Store) DeleteLabel(key string) error {
	_, err := s.db.Exec(`DELETE FROM labels WHERE key=?`, key)
	return err
}

// Prices: daily USD close in cents per BTC.

func (s *Store) PutPrices(p map[string]int64, source string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for day, c := range p {
		if _, err := tx.Exec(`INSERT INTO prices(day,cents,source) VALUES(?,?,?) ON CONFLICT(day) DO UPDATE SET cents=excluded.cents, source=excluded.source`, day, c, source); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Prices() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT day,cents FROM prices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var d string
		var c int64
		if err := rows.Scan(&d, &c); err != nil {
			return nil, err
		}
		out[d] = c
	}
	return out, rows.Err()
}

// Wipe deletes everything: settings, accounts, imports, scans, labels,
// prices and caches. The file is then compacted so deleted rows do not
// linger in free pages or the write-ahead log.
func (s *Store) Wipe() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"labels", "events", "imports", "wallet_txs", "accounts", "raw_txs", "headers", "prices", "settings"} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return err
	}
	_, err = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
