package app

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/store"
)

// A backup is one JSON file. Its payload is every table, as JSON, gzipped
// and, by default, encrypted with AES-256-GCM under a key derived from the
// passphrase (PBKDF2-SHA256). sha256 is over the uncompressed JSON, so a
// restore can tell a damaged file from a wrong passphrase.

const (
	backupFormat  = "btctax-backup"
	backupVersion = 1
	pbkdfIter     = 600_000
)

type backupFile struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	Created   string `json:"created"`
	Encrypted bool   `json:"encrypted"`
	KDF       string `json:"kdf,omitempty"`
	Iter      int    `json:"iter,omitempty"`
	Salt      string `json:"salt,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	SHA256    string `json:"sha256"`
	Payload   string `json:"payload"` // base64 of gzip(JSON), encrypted when Encrypted
}

func backupKey(pass string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pass, salt, iter, 32)
}

func makeBackup(t store.Tables, pass string) ([]byte, error) {
	plain, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(plain)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain)
	zw.Close()
	f := backupFile{Format: backupFormat, Version: backupVersion, Created: time.Now().UTC().Format(time.RFC3339), SHA256: hex.EncodeToString(sum[:])}
	data := gz.Bytes()
	if pass != "" {
		salt, nonce := make([]byte, 16), make([]byte, 12)
		rand.Read(salt)
		rand.Read(nonce)
		key, err := backupKey(pass, salt, pbkdfIter)
		if err != nil {
			return nil, err
		}
		block, _ := aes.NewCipher(key)
		gcm, _ := cipher.NewGCM(block)
		header := []byte(backupFormat) // binds the ciphertext to this format
		data = gcm.Seal(nil, nonce, data, header)
		f.Encrypted, f.KDF, f.Iter = true, "pbkdf2-sha256", pbkdfIter
		f.Salt, f.Nonce = base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(nonce)
	}
	f.Payload = base64.StdEncoding.EncodeToString(data)
	return json.MarshalIndent(f, "", " ")
}

var errPassphrase = errors.New("wrong passphrase, or the file was changed")

func readBackup(b []byte, pass string) (store.Tables, error) {
	var f backupFile
	if err := json.Unmarshal(b, &f); err != nil || f.Format != backupFormat {
		return nil, errors.New("this isn't a Proof of Basis backup file")
	}
	if f.Version < 1 || f.Version > backupVersion {
		return nil, fmt.Errorf("this backup is version %d; this app reads up to version %d, so update the app first", f.Version, backupVersion)
	}
	data, err := base64.StdEncoding.DecodeString(f.Payload)
	if err != nil {
		return nil, errors.New("the backup file is damaged")
	}
	if f.Encrypted {
		if pass == "" {
			return nil, errors.New("this backup is encrypted: enter its passphrase")
		}
		if f.KDF != "pbkdf2-sha256" || f.Iter < 100_000 || f.Iter > 10_000_000 {
			return nil, errors.New("the backup uses an encryption setting this app doesn't know")
		}
		salt, err1 := base64.StdEncoding.DecodeString(f.Salt)
		nonce, err2 := base64.StdEncoding.DecodeString(f.Nonce)
		if err1 != nil || err2 != nil || len(nonce) != 12 {
			return nil, errors.New("the backup file is damaged")
		}
		key, err := backupKey(pass, salt, f.Iter)
		if err != nil {
			return nil, err
		}
		block, _ := aes.NewCipher(key)
		gcm, _ := cipher.NewGCM(block)
		if data, err = gcm.Open(nil, nonce, data, []byte(backupFormat)); err != nil {
			return nil, errPassphrase
		}
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the backup file is damaged")
	}
	plain, err := io.ReadAll(io.LimitReader(zr, 2<<30))
	if err != nil {
		return nil, errors.New("the backup file is damaged")
	}
	if sum := sha256.Sum256(plain); hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, errors.New("the backup failed its integrity check: the file is damaged or was edited")
	}
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.UseNumber()
	var t store.Tables
	if err := dec.Decode(&t); err != nil {
		return nil, errors.New("the backup file is damaged")
	}
	return t, nil
}

// postBackup returns the backup file for download. The passphrase comes in
// the body, never the URL.
func (s *Server) postBackup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Passphrase  string `json:"passphrase"`
		Unencrypted bool   `json:"unencrypted"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if !in.Unencrypted && len(in.Passphrase) < 8 {
		fail(w, 400, errors.New("choose a passphrase of at least 8 characters, or turn encryption off"))
		return
	}
	if in.Unencrypted {
		in.Passphrase = ""
	}
	s.scanMu.Lock()
	t, err := s.st.Dump()
	s.scanMu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	b, err := makeBackup(t, in.Passphrase)
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="proof-of-basis-backup-%s.json"`, time.Now().Format("2006-01-02")))
	w.Write(b)
}

// postRestore replaces all data with a backup's. It needs confirm=REPLACE,
// checks version and integrity first, and saves a copy of the current
// database beside it before replacing anything.
func (s *Server) postRestore(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		fail(w, 400, err)
		return
	}
	if r.FormValue("confirm") != "REPLACE" {
		fail(w, 400, errors.New(`restoring replaces all current data; confirm with "REPLACE"`))
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, errors.New("choose a backup file"))
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<30))
	if err != nil {
		fail(w, 400, err)
		return
	}
	t, err := readBackup(b, r.FormValue("passphrase"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	snap, err := s.st.Snapshot("before-restore")
	if err != nil {
		fail(w, 500, fmt.Errorf("couldn't save a copy of the current data first, so nothing was changed: %w", err))
		return
	}
	if err := s.st.Restore(t); err != nil {
		fail(w, 400, fmt.Errorf("nothing was changed: %w", err))
		return
	}
	s.statusMu.Lock()
	s.status = fulcrumStatus{}
	s.statusMu.Unlock()
	counts := map[string]int{}
	for k, v := range t {
		counts[k] = len(v)
	}
	writeJSON(w, map[string]any{"ok": true, "rows": counts, "previousCopy": snap != ""})
}
