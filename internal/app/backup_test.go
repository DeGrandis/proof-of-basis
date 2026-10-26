package app

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DeGrandis/proof-of-basis/internal/demo"
)

func restore(h http.Handler, file []byte, pass, confirm string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "backup.json")
	fw.Write(file)
	mw.WriteField("passphrase", pass)
	mw.WriteField("confirm", confirm)
	mw.Close()
	r := httptest.NewRequest("POST", "/api/restore", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// stable is the part of a report that must survive a round trip exactly.
func stable(t *testing.T, h http.Handler) string {
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	b, _ := json.Marshal([]any{rep.Accounts, rep.Disposals, rep.Removals, rep.Income, rep.Unmatched, rep.Links, rep.Graph, rep.Warnings, rep.Years, rep.Settings})
	return string(b)
}

// Backup, wipe, restore: the made-up history, labels, protected lots,
// settings and the Lightning rune all come back exactly.
func TestBackupRoundTrip(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 2); err != nil {
		t.Fatal(err)
	}
	call(h, "POST", "/api/settings", `{"fulcrumHost":"node.local","clnUrl":"https://node.local:3010","clnRune":"made-up-rune"}`)
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	for _, g := range rep.Graph {
		if g.Kind == "buy" {
			call(h, "POST", "/api/protect", `{"key":"`+g.Key+`","on":true}`)
			break
		}
	}
	before := stable(t, h)

	if w := call(h, "POST", "/api/backup", `{"passphrase":"short"}`); w.Code != 400 {
		t.Errorf("short passphrase accepted: %d", w.Code)
	}
	enc := call(h, "POST", "/api/backup", `{"passphrase":"correct horse battery"}`)
	if enc.Code != 200 || strings.Contains(enc.Body.String(), "made-up-rune") || strings.Contains(enc.Body.String(), "demo:Cold") {
		t.Fatalf("encrypted backup leaks or failed: %d", enc.Code)
	}
	plain := call(h, "POST", "/api/backup", `{"unencrypted":true}`)
	if plain.Code != 200 {
		t.Fatal(plain.Body.String())
	}

	call(h, "POST", "/api/wipe", `{"confirm":"WIPE"}`)
	if w := restore(h, enc.Body.Bytes(), "correct horse battery", ""); w.Code != 400 {
		t.Errorf("restored without confirmation")
	}
	if w := restore(h, enc.Body.Bytes(), "wrong passphrase", "REPLACE"); w.Code != 400 || !strings.Contains(w.Body.String(), "passphrase") {
		t.Errorf("wrong passphrase: %d %s", w.Code, w.Body.String())
	}
	tampered := bytes.Replace(plain.Body.Bytes(), []byte(`"sha256": "`), []byte(`"sha256": "00`), 1)
	if w := restore(h, tampered, "", "REPLACE"); w.Code != 400 || !strings.Contains(w.Body.String(), "integrity") {
		t.Errorf("tampered file: %d %s", w.Code, w.Body.String())
	}
	future := bytes.Replace(plain.Body.Bytes(), []byte(`"version": 1`), []byte(`"version": 9`), 1)
	if w := restore(h, future, "", "REPLACE"); w.Code != 400 || !strings.Contains(w.Body.String(), "version") {
		t.Errorf("future version: %d %s", w.Code, w.Body.String())
	}
	if w := restore(h, enc.Body.Bytes(), "correct horse battery", "REPLACE"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if after := stable(t, h); after != before {
		i := 0
		for i < len(after) && i < len(before) && after[i] == before[i] {
			i++
		}
		lo := max(i-200, 0)
		t.Errorf("encrypted round trip changed the data at %d:\nbefore %s\nafter  %s", i, before[lo:min(i+200, len(before))], after[lo:min(i+200, len(after))])
	}
	if st.Setting("clnRune", "") != "made-up-rune" {
		t.Error("rune not restored")
	}
	// The unencrypted file restores the same state over the current one.
	if w := restore(h, plain.Body.Bytes(), "", "REPLACE"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if after := stable(t, h); after != before {
		t.Error("unencrypted round trip changed the data")
	}
	// Wipe everything also removes the copies kept before each restore.
	call(h, "POST", "/api/wipe", `{"confirm":"WIPE"}`)
	if left, _ := filepath.Glob(filepath.Join(t.TempDir(), "..", "*", "t.db.before-restore-*")); len(left) != 0 {
		t.Errorf("copies left after wipe: %v", left)
	}
}
