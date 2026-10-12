package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Tables holds every row of every table, for a backup. Rows are column
// name to value; values are what SQLite returns (int64, float64, string,
// []byte or nil).
type Tables map[string][]map[string]any

// backupTables lists what a backup holds, parents before children so a
// restore satisfies foreign keys.
var backupTables = []string{"settings", "accounts", "imports", "events", "wallet_txs", "raw_txs", "headers", "labels", "prices", "ln_events"}

// Dump reads every table into memory.
func (s *Store) Dump() (Tables, error) {
	out := Tables{}
	for _, t := range backupTables {
		rows, err := s.db.Query(`SELECT * FROM ` + t)
		if err != nil {
			return nil, err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, err
		}
		list := []map[string]any{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			row := map[string]any{}
			for i, c := range cols {
				if b, ok := vals[i].([]byte); ok {
					vals[i] = string(b) // every column here is text or integer
				}
				row[c] = vals[i]
			}
			list = append(list, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out[t] = list
	}
	return out, nil
}

func (s *Store) columns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols[c] = true
	}
	return cols, rows.Err()
}

// Snapshot writes a copy of the whole database next to it, as a safety net
// before a restore, and returns its path.
func (s *Store) Snapshot(suffix string) (string, error) {
	if s.path == "" {
		return "", nil
	}
	now := time.Now().UTC()
	p := fmt.Sprintf("%s.%s-%s-%09d", s.path, suffix, now.Format("20060102-150405"), now.Nanosecond())
	_, err := s.db.Exec(`VACUUM INTO ?`, p)
	return p, err
}

// Restore replaces every table with the given rows in one transaction:
// either all of it lands or nothing changes. Columns the current schema
// doesn't know are an error; columns the backup lacks take their defaults.
func (s *Store) Restore(t Tables) error {
	for name := range t {
		known := false
		for _, b := range backupTables {
			known = known || b == name
		}
		if !known {
			return fmt.Errorf("backup has an unknown table %q", name)
		}
	}
	if len(t["settings"]) == 0 && len(t["accounts"]) == 0 && len(t["events"]) == 0 {
		return errors.New("the backup is empty")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := len(backupTables) - 1; i >= 0; i-- { // children first
		if _, err := tx.Exec(`DELETE FROM ` + backupTables[i]); err != nil {
			return err
		}
	}
	for _, name := range backupTables {
		cols, err := s.columns(tx, name)
		if err != nil {
			return err
		}
		for _, row := range t[name] {
			var names, marks []string
			var vals []any
			for c, v := range row {
				if !cols[c] {
					return fmt.Errorf("backup table %s has a column %q this version doesn't know", name, c)
				}
				switch n := v.(type) {
				case json.Number:
					if i, err := n.Int64(); err == nil {
						v = i
					} else if f, err := n.Float64(); err == nil {
						v = f
					}
				case float64:
					if n == float64(int64(n)) {
						v = int64(n)
					}
				}
				names, marks, vals = append(names, `"`+c+`"`), append(marks, "?"), append(vals, v)
			}
			if len(names) == 0 {
				continue
			}
			q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, name, strings.Join(names, ","), strings.Join(marks, ","))
			if _, err := tx.Exec(q, vals...); err != nil {
				return fmt.Errorf("restore %s: %w", name, err)
			}
		}
	}
	return tx.Commit()
}
