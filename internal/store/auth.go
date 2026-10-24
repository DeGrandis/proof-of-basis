package store

import "time"

// Login data lives in its own tables, outside settings: a wipe or a
// restore never changes the password or signs anyone out, and a backup
// never carries the password hash.
const authSchema = `
CREATE TABLE IF NOT EXISTS auth (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (hash TEXT PRIMARY KEY, created INTEGER NOT NULL, seen INTEGER NOT NULL);
`

// PasswordHash is the stored password hash, or "" before one is set.
func (s *Store) PasswordHash() string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM auth WHERE key='password'`).Scan(&v); err != nil {
		return ""
	}
	return v
}

// SetPasswordHash replaces the password and signs out every session.
// An empty hash clears the password, so the next visit sets a new one.
func (s *Store) SetPasswordHash(h string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if h == "" {
		_, err = tx.Exec(`DELETE FROM auth WHERE key='password'`)
	} else {
		_, err = tx.Exec(`INSERT INTO auth(key,value) VALUES('password',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, h)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	return tx.Commit()
}

// AddSession stores a session by the hash of its token.
func (s *Store) AddSession(hash string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO sessions(hash,created,seen) VALUES(?,?,?)`, hash, now.Unix(), now.Unix())
	return err
}

// TouchSession reports whether a session exists and was used within
// idle. If so and it was last seen more than a minute ago, it is marked
// seen now (renewed is then true) so active sessions don't expire.
func (s *Store) TouchSession(hash string, now time.Time, idle time.Duration) (ok, renewed bool) {
	var seen int64
	if err := s.db.QueryRow(`SELECT seen FROM sessions WHERE hash=?`, hash).Scan(&seen); err != nil {
		return false, false
	}
	if now.Sub(time.Unix(seen, 0)) > idle {
		s.db.Exec(`DELETE FROM sessions WHERE hash=?`, hash)
		return false, false
	}
	if now.Unix()-seen > 60 {
		s.db.Exec(`UPDATE sessions SET seen=? WHERE hash=?`, now.Unix(), hash)
		return true, true
	}
	return true, false
}

func (s *Store) DeleteSession(hash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE hash=?`, hash)
	return err
}

func (s *Store) DeleteSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions`)
	return err
}
