package app

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"btctax/internal/importers"

	"btctax/internal/store"
)

// Files kept as proof for a record: listed, downloaded unchanged, never
// served as a page, and removable. Invented files.
func TestAttachments(t *testing.T) {
	st, h := newTest(t)
	upload := func(name string, data []byte) store.Attachment {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("key", "ex:7")
		mw.WriteField("note", "charity letter")
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(data)
		mw.Close()
		req := httptest.NewRequest("POST", "/api/attachments", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("upload %s: %d %s", name, w.Code, w.Body.String())
		}
		var a store.Attachment
		json.Unmarshal(w.Body.Bytes(), &a)
		return a
	}
	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte{0, 1, 2, 250, 255}, 50)...)
	a := upload("..\\letters\\receipt.pdf", pdf)
	b := upload("page.html", []byte("<html><script>alert(1)</script></html>"))
	if a.Filename != "receipt.pdf" || a.Mime != "application/pdf" || a.Size != int64(len(pdf)) {
		t.Errorf("pdf %+v", a)
	}
	var list []store.Attachment
	json.Unmarshal(call(h, "GET", "/api/attachments?key=ex:7", "").Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("list %+v", list)
	}
	w := call(h, "GET", "/api/attachments/"+strconv.FormatInt(a.ID, 10), "")
	if !bytes.Equal(w.Body.Bytes(), pdf) || w.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("pdf came back changed or retyped: %s", w.Header().Get("Content-Type"))
	}
	w = call(h, "GET", "/api/attachments/"+strconv.FormatInt(b.ID, 10), "")
	if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Content-Disposition") == "" {
		t.Errorf("html served as %q", w.Header().Get("Content-Type"))
	}
	// Backups carry the file intact.
	tables, err := st.Dump()
	if err != nil || len(tables["attachments"]) != 2 {
		t.Fatalf("backup: %v %d", err, len(tables["attachments"]))
	}
	call(h, "DELETE", "/api/attachments/"+strconv.FormatInt(b.ID, 10), "")
	json.Unmarshal(call(h, "GET", "/api/attachments?key=ex:7", "").Body.Bytes(), &list)
	if len(list) != 1 {
		t.Errorf("after delete %+v", list)
	}
}

// An imported record's original row, as the exchange's file had it.
func TestRecordSource(t *testing.T) {
	st, h := newTest(t)
	cb, _ := st.ExchangeAccount("Coinbase", "coinbase")
	st.AddImport(cb, "coinbase", "coinbase-2025.csv", "x", []importers.Event{{Kind: importers.Buy, Time: time.Unix(1_700_000_000, 0), Sats: 1000, FiatCents: 100,
		UID: "c1", Row: 5, Raw: map[string]string{"ID": "c1", "Transaction Type": "Buy", "Quantity Transacted": "0.00001"}}}, nil)
	var src store.EventSource
	w := call(h, "GET", "/api/records/ex:1/source", "")
	json.Unmarshal(w.Body.Bytes(), &src)
	if w.Code != 200 || src.Filename != "coinbase-2025.csv" || src.Line != 5 || src.Row["Transaction Type"] != "Buy" || src.Account != "Coinbase" {
		t.Errorf("%d %+v", w.Code, src)
	}
	if w := call(h, "GET", "/api/records/tx:abc:0/source", ""); w.Code != 400 {
		t.Errorf("chain record: %d", w.Code)
	}
}
