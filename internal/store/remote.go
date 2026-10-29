package store

// RemoteBackup returns the automatic backup's settings and state.
func (s *Store) RemoteBackup() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM remote_backup`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// SetRemoteBackup saves the given keys in one transaction; an empty value
// removes the key.
func (s *Store) SetRemoteBackup(m map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range m {
		if v == "" {
			_, err = tx.Exec(`DELETE FROM remote_backup WHERE key=?`, k)
		} else {
			_, err = tx.Exec(`INSERT INTO remote_backup(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
