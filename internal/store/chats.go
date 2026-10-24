package store

import (
	"database/sql"
	"errors"
	"time"
)

// Assistant chats. They are part of your data: included in backups and
// removed by a wipe.
const chatSchema = `
CREATE TABLE IF NOT EXISTS chats (
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL,
  updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS chat_messages (
  id INTEGER PRIMARY KEY,
  chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS chat_messages_chat ON chat_messages(chat_id, id);
`

type Chat struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
}

type ChatMessage struct {
	ID      int64  `json:"id"`
	Role    string `json:"role"` // user or assistant
	Content string `json:"content"`
	Data    string `json:"data,omitempty"` // JSON: lookups, page, model
	Created int64  `json:"created"`
}

var ErrNoChat = errors.New("that chat doesn't exist")

func (s *Store) Chats() ([]Chat, error) {
	rows, err := s.db.Query(`SELECT id, title, model, created, updated FROM chats ORDER BY updated DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Title, &c.Model, &c.Created, &c.Updated); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Chat(id int64) (Chat, []ChatMessage, error) {
	var c Chat
	err := s.db.QueryRow(`SELECT id, title, model, created, updated FROM chats WHERE id=?`, id).Scan(&c.ID, &c.Title, &c.Model, &c.Created, &c.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil, ErrNoChat
	} else if err != nil {
		return c, nil, err
	}
	rows, err := s.db.Query(`SELECT id, role, content, data, created FROM chat_messages WHERE chat_id=? ORDER BY id`, id)
	if err != nil {
		return c, nil, err
	}
	defer rows.Close()
	msgs := []ChatMessage{}
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Data, &m.Created); err != nil {
			return c, nil, err
		}
		msgs = append(msgs, m)
	}
	return c, msgs, rows.Err()
}

func (s *Store) NewChat(title, model string, now time.Time) (int64, error) {
	r, err := s.db.Exec(`INSERT INTO chats(title, model, created, updated) VALUES(?,?,?,?)`, title, model, now.Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) RenameChat(id int64, title string) error {
	r, err := s.db.Exec(`UPDATE chats SET title=? WHERE id=?`, title, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNoChat
	}
	return nil
}

func (s *Store) AddChatMessage(chatID int64, m ChatMessage, model string, now time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT INTO chat_messages(chat_id, role, content, data, created) VALUES(?,?,?,?,?)`, chatID, m.Role, m.Content, m.Data, now.Unix())
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE chats SET updated=?, model=CASE WHEN ?='' THEN model ELSE ? END WHERE id=?`, now.Unix(), model, model, chatID); err != nil {
		return 0, err
	}
	id, _ := r.LastInsertId()
	return id, tx.Commit()
}

func (s *Store) DeleteChat(id int64) error {
	_, err := s.db.Exec(`DELETE FROM chats WHERE id=?`, id)
	return err
}

func (s *Store) DeleteChats() error {
	_, err := s.db.Exec(`DELETE FROM chats`)
	return err
}
