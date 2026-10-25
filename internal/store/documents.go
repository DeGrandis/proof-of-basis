package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	"btctax/internal/importers"
)

// Documents a tax professional needs alongside the records: brokers'
// Forms 1099-DA, lot identifications made before a sale, and files the
// user attaches as proof (receipts, donation letters, gift notes).
const docSchema = `
CREATE TABLE IF NOT EXISTS forms_1099da (
  id INTEGER PRIMARY KEY,
  account TEXT NOT NULL,
  filename TEXT NOT NULL,
  imported_at TEXT NOT NULL,
  rows TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS identifications (
  id INTEGER PRIMARY KEY,
  created_at TEXT NOT NULL,
  data TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS attachments (
  id INTEGER PRIMARY KEY,
  key TEXT NOT NULL,
  filename TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  added_at TEXT NOT NULL,
  data TEXT NOT NULL -- base64, so backups (all text) keep it intact
);
CREATE INDEX IF NOT EXISTS attachments_key ON attachments(key);
`

// DAForm is one uploaded 1099-DA export for one account.
type DAForm struct {
	ID         int64               `json:"id"`
	Account    string              `json:"account"`
	Filename   string              `json:"filename"`
	ImportedAt string              `json:"importedAt"`
	Rows       []importers.DARow   `json:"rows"`
	Warnings   []importers.Warning `json:"warnings,omitempty"`
}

func (s *Store) Add1099DA(account, filename string, rows []importers.DARow) (int64, error) {
	b, _ := json.Marshal(rows)
	r, err := s.db.Exec(`INSERT INTO forms_1099da(account, filename, imported_at, rows) VALUES(?,?,?,?)`,
		account, filename, time.Now().UTC().Format(time.RFC3339), string(b))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) Forms1099DA() ([]DAForm, error) {
	rows, err := s.db.Query(`SELECT id, account, filename, imported_at, rows FROM forms_1099da ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DAForm{}
	for rows.Next() {
		var f DAForm
		var data string
		if err := rows.Scan(&f.ID, &f.Account, &f.Filename, &f.ImportedAt, &data); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(data), &f.Rows)
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) Delete1099DA(id int64) error {
	_, err := s.db.Exec(`DELETE FROM forms_1099da WHERE id=?`, id)
	return err
}

// Identification is which coins the user chose to sell or spend, recorded
// before the sale (IRS FAQ 82: no later than the date and time of the
// sale). Picks name lots by the acquisition they came from.
type Identification struct {
	ID        int64         `json:"id"`
	CreatedAt time.Time     `json:"createdAt"`
	Account   string        `json:"account"`
	Sats      int64         `json:"sats"`
	Picks     []PickOrigin  `json:"picks"`
	Strategy  string        `json:"strategy,omitempty"`
	Note      string        `json:"note,omitempty"`
	AppliesTo string        `json:"appliesTo,omitempty"` // a record key, when the user tied it to one sale
	Cancelled bool          `json:"cancelled,omitempty"`
	Estimate  *IdentifyPlan `json:"estimate,omitempty"`
}

// PickOrigin is part of an acquisition chosen for a sale.
type PickOrigin struct {
	OriginID string    `json:"originId"`
	Acquired time.Time `json:"acquired"`
	Sats     int64     `json:"sats"`
	Basis    int64     `json:"basis"`
}

// IdentifyPlan is what the planner showed when the choice was recorded.
type IdentifyPlan struct {
	Price    int64 `json:"price"`
	Proceeds int64 `json:"proceeds"`
	Basis    int64 `json:"basis"`
	ShortSat int64 `json:"shortSats"` // sats that would be short-term
	LongSat  int64 `json:"longSats"`
	ExtraTax int64 `json:"extraTax"`
}

func (s *Store) AddIdentification(id Identification) (int64, error) {
	id.CreatedAt = id.CreatedAt.UTC()
	b, _ := json.Marshal(id)
	r, err := s.db.Exec(`INSERT INTO identifications(created_at, data) VALUES(?,?)`, id.CreatedAt.Format(time.RFC3339Nano), string(b))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) Identifications() ([]Identification, error) {
	rows, err := s.db.Query(`SELECT id, data FROM identifications ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identification{}
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var x Identification
		json.Unmarshal([]byte(data), &x)
		x.ID = id
		out = append(out, x)
	}
	return out, rows.Err()
}

// UpdateIdentification changes what an identification applies to or
// cancels it. The choice itself and its time are never rewritten, so the
// record stays proof of what was decided before the sale.
func (s *Store) UpdateIdentification(id int64, appliesTo string, cancelled bool) error {
	var data string
	if err := s.db.QueryRow(`SELECT data FROM identifications WHERE id=?`, id).Scan(&data); err != nil {
		return err
	}
	var x Identification
	json.Unmarshal([]byte(data), &x)
	x.AppliesTo, x.Cancelled = appliesTo, cancelled
	b, _ := json.Marshal(x)
	_, err := s.db.Exec(`UPDATE identifications SET data=? WHERE id=?`, string(b), id)
	return err
}

// Attachment is a file kept as proof for one record.
type Attachment struct {
	ID       int64  `json:"id"`
	Key      string `json:"key"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Note     string `json:"note,omitempty"`
	AddedAt  string `json:"addedAt"`
}

func (s *Store) AddAttachment(key, filename, mime, note string, data []byte) (Attachment, error) {
	sum := sha256.Sum256(data)
	a := Attachment{Key: key, Filename: filename, Mime: mime, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Note: note,
		AddedAt: time.Now().UTC().Format(time.RFC3339)}
	r, err := s.db.Exec(`INSERT INTO attachments(key, filename, mime, size, sha256, note, added_at, data) VALUES(?,?,?,?,?,?,?,?)`,
		a.Key, a.Filename, a.Mime, a.Size, a.SHA256, a.Note, a.AddedAt, base64.StdEncoding.EncodeToString(data))
	if err != nil {
		return a, err
	}
	a.ID, _ = r.LastInsertId()
	return a, nil
}

// Attachments lists attachments (without their contents), for one record
// key or for all when key is empty.
func (s *Store) Attachments(key string) ([]Attachment, error) {
	q, args := `SELECT id, key, filename, mime, size, sha256, note, added_at FROM attachments ORDER BY id`, []any{}
	if key != "" {
		q, args = `SELECT id, key, filename, mime, size, sha256, note, added_at FROM attachments WHERE key=? ORDER BY id`, []any{key}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.Key, &a.Filename, &a.Mime, &a.Size, &a.SHA256, &a.Note, &a.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AttachmentData(id int64) (Attachment, []byte, error) {
	var a Attachment
	var enc string
	err := s.db.QueryRow(`SELECT id, key, filename, mime, size, sha256, note, added_at, data FROM attachments WHERE id=?`, id).
		Scan(&a.ID, &a.Key, &a.Filename, &a.Mime, &a.Size, &a.SHA256, &a.Note, &a.AddedAt, &enc)
	if err != nil {
		return a, nil, err
	}
	data, err := base64.StdEncoding.DecodeString(enc)
	return a, data, err
}

func (s *Store) DeleteAttachment(id int64) error {
	_, err := s.db.Exec(`DELETE FROM attachments WHERE id=?`, id)
	return err
}
