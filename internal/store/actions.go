package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The activity log: every change made by the user or the assistant, with
// enough before/after state to undo it. It stays in this database (not in
// backups) and a wipe clears it.
const actionSchema = `
CREATE TABLE IF NOT EXISTS actions (
  id INTEGER PRIMARY KEY,
  created INTEGER NOT NULL,
  updated INTEGER NOT NULL,
  actor TEXT NOT NULL,          -- user or assistant
  kind TEXT NOT NULL,
  summary TEXT NOT NULL,
  changes TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL,         -- done, undone, pending, dismissed, info
  chat_id INTEGER NOT NULL DEFAULT 0,
  undoable INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS actions_created ON actions(created DESC, id DESC);
`

type Action struct {
	ID       int64           `json:"id"`
	Created  int64           `json:"created"`
	Updated  int64           `json:"updated"`
	Actor    string          `json:"actor"`
	Kind     string          `json:"kind"`
	Summary  string          `json:"summary"`
	Changes  json.RawMessage `json:"changes"`
	Status   string          `json:"status"`
	ChatID   int64           `json:"chatId,omitempty"`
	Undoable bool            `json:"undoable"`
}

var ErrNoAction = errors.New("that action doesn't exist")

func (s *Store) AddAction(a Action) (int64, error) {
	if len(a.Changes) == 0 {
		a.Changes = json.RawMessage("[]")
	}
	now := time.Now().Unix()
	r, err := s.db.Exec(`INSERT INTO actions(created, updated, actor, kind, summary, changes, status, chat_id, undoable) VALUES(?,?,?,?,?,?,?,?,?)`,
		now, now, a.Actor, a.Kind, a.Summary, string(a.Changes), a.Status, a.ChatID, a.Undoable)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func scanAction(sc interface{ Scan(...any) error }) (Action, error) {
	var a Action
	var ch string
	err := sc.Scan(&a.ID, &a.Created, &a.Updated, &a.Actor, &a.Kind, &a.Summary, &ch, &a.Status, &a.ChatID, &a.Undoable)
	a.Changes = json.RawMessage(ch)
	return a, err
}

const actionCols = `id, created, updated, actor, kind, summary, changes, status, chat_id, undoable`

func (s *Store) Action(id int64) (Action, error) {
	a, err := scanAction(s.db.QueryRow(`SELECT `+actionCols+` FROM actions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNoAction
	}
	return a, err
}

// Actions lists the newest first. actor filters when not empty.
func (s *Store) Actions(actor string, limit int) ([]Action, error) {
	q, args := `SELECT `+actionCols+` FROM actions`, []any{}
	if actor != "" {
		q += ` WHERE actor=?`
		args = append(args, actor)
	}
	q += ` ORDER BY created DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) UpdateAction(id int64, status string, changes json.RawMessage) error {
	if changes == nil {
		_, err := s.db.Exec(`UPDATE actions SET status=?, updated=? WHERE id=?`, status, time.Now().Unix(), id)
		return err
	}
	_, err := s.db.Exec(`UPDATE actions SET status=?, changes=?, updated=? WHERE id=?`, status, string(changes), time.Now().Unix(), id)
	return err
}

// LabelRaw is a label's stored JSON, or "" when there is none.
func (s *Store) LabelRaw(key string) string {
	var v string
	if err := s.db.QueryRow(`SELECT data FROM labels WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

func (s *Store) SetLabelRaw(key, data string) error {
	_, err := s.db.Exec(`INSERT INTO labels(key,data) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, key, data)
	return err
}

// SettingRaw reports a setting's stored value and whether it is stored.
func (s *Store) SettingRaw(key string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, key)
	return err
}

// RowSet is the rows of one table matching Col = Val, for undoing adds and
// deletions of accounts and imports.
type RowSet struct {
	Table string           `json:"table"`
	Col   string           `json:"col"`
	Val   any              `json:"val"`
	Rows  []map[string]any `json:"rows"`
}

var rowTables = map[string]map[string]bool{
	"accounts":   {"id": true},
	"imports":    {"id": true, "account_id": true},
	"events":     {"import_id": true, "account_id": true},
	"wallet_txs": {"account_id": true},
}

func checkSet(rs RowSet) error {
	if !rowTables[rs.Table][rs.Col] {
		return fmt.Errorf("can't snapshot %s.%s", rs.Table, rs.Col)
	}
	return nil
}

func numVal(v any) any {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	case float64:
		if n == float64(int64(n)) {
			return int64(n)
		}
	}
	return v
}

// CaptureRows fills each set with the rows it matches now.
func (s *Store) CaptureRows(sets []RowSet) ([]RowSet, error) {
	out := make([]RowSet, len(sets))
	for i, rs := range sets {
		if err := checkSet(rs); err != nil {
			return nil, err
		}
		rows, err := s.db.Query(`SELECT * FROM `+rs.Table+` WHERE `+rs.Col+`=? ORDER BY rowid`, numVal(rs.Val))
		if err != nil {
			return nil, err
		}
		cols, _ := rows.Columns()
		list := []map[string]any{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for j := range vals {
				ptrs[j] = &vals[j]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			row := map[string]any{}
			for j, c := range cols {
				if b, ok := vals[j].([]byte); ok {
					vals[j] = string(b)
				}
				row[c] = vals[j]
			}
			list = append(list, row)
		}
		rows.Close()
		out[i] = RowSet{Table: rs.Table, Col: rs.Col, Val: rs.Val, Rows: list}
	}
	return out, nil
}

// ReplaceRows makes each set match exactly its Rows: the current matches
// are deleted (children first) and the saved rows inserted (parents
// first). Sets are given parents first.
func (s *Store) ReplaceRows(sets []RowSet) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := len(sets) - 1; i >= 0; i-- {
		rs := sets[i]
		if err := checkSet(rs); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM `+rs.Table+` WHERE `+rs.Col+`=?`, numVal(rs.Val)); err != nil {
			return err
		}
	}
	for _, rs := range sets {
		for _, row := range rs.Rows {
			var names, marks []string
			var vals []any
			for c, v := range row {
				if strings.ContainsAny(c, "\"`;") {
					return fmt.Errorf("bad column %q", c)
				}
				names, marks, vals = append(names, `"`+c+`"`), append(marks, "?"), append(vals, numVal(v))
			}
			if _, err := tx.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (%s) VALUES (%s)`, rs.Table, strings.Join(names, ","), strings.Join(marks, ",")), vals...); err != nil {
				return fmt.Errorf("restore %s: %w", rs.Table, err)
			}
		}
	}
	return tx.Commit()
}

// AccountByName finds an account's id.
func (s *Store) AccountByName(name string) (int64, bool) {
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM accounts WHERE name=?`, name).Scan(&id); err != nil {
		return 0, false
	}
	return id, true
}
