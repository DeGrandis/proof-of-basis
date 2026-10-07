package app

import (
	"btctax/internal/demo"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"btctax/internal/store"
)

// BIP84 test vector key (public test data, not anyone's wallet).
const zpub = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"
const xpub44 = "xpub6BosfCnifzxcFwrSzQiqu2DBVTshkCXacvNsWGYJVVhhawA7d4R5WSWGFNbi8Aw6ZRc1brxMyWMzG3DSSSSoekkudhUd9yLb6qx39T9nMdj"

func newTest(t *testing.T) (*store.Store, http.Handler) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, New(st, fstest.MapFS{}, "").Handler()
}

func call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEmptyReportHasNoNullLists(t *testing.T) {
	_, h := newTest(t)
	w := call(h, "GET", "/api/report", "")
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"accounts", "years", "disposals", "removals", "income", "unmatched", "links", "graph", "warnings"} {
		if _, ok := m[k].([]any); !ok {
			t.Errorf("%s is %v, want a list", k, m[k])
		}
	}
}

func TestSameWalletRefused(t *testing.T) {
	_, h := newTest(t)
	if w := call(h, "POST", "/api/wallets", `{"name":"Cold","key":"`+zpub+`"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, body := range []string{
		`{"name":"Cold again","key":"` + zpub + `"}`,
		`{"name":"Cold again","key":"` + zpub + `","script":"p2wpkh"}`,
		`{"name":"cold","key":"` + xpub44 + `"}`, // name clash, any case
	} {
		if w := call(h, "POST", "/api/wallets", body); w.Code != 409 {
			t.Errorf("%s: got %d %s", body, w.Code, w.Body.String())
		}
	}
	// The same key with a different script type is a different wallet.
	if w := call(h, "POST", "/api/wallets", `{"name":"Legacy","key":"`+xpub44+`"}`); w.Code != 200 {
		t.Error(w.Body.String())
	}
	if w := call(h, "POST", "/api/wallets", `{"name":"Legacy segwit","key":"`+xpub44+`","script":"p2wpkh"}`); w.Code != 200 {
		t.Error(w.Body.String())
	}
}

func TestWipe(t *testing.T) {
	st, h := newTest(t)
	call(h, "POST", "/api/settings", `{"fulcrumHost":"node.local"}`)
	call(h, "POST", "/api/wallets", `{"name":"Cold","key":"`+zpub+`"}`)
	if w := call(h, "POST", "/api/wipe", `{}`); w.Code != 400 {
		t.Fatalf("wipe without confirmation: %d", w.Code)
	}
	if w := call(h, "POST", "/api/wipe", `{"confirm":"WIPE"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if a, _ := st.Accounts(); len(a) != 0 || st.Setting("fulcrumHost", "") != "" {
		t.Errorf("left behind: %v %q", a, st.Setting("fulcrumHost", ""))
	}
}

func upload(h http.Handler, name, content, handler string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	mw.WriteField("handler", handler)
	mw.Close()
	r := httptest.NewRequest("POST", "/api/imports", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The drop zone relies on these codes: 422 means "ask for the platform",
// 409 means "already imported".
func TestImportStatusCodes(t *testing.T) {
	_, h := newTest(t)
	river, err := os.ReadFile("../importers/testdata/river.csv")
	if err != nil {
		t.Fatal(err)
	}
	if w := upload(h, "river.csv", string(river), ""); w.Code != 200 {
		t.Fatalf("detect: %d %s", w.Code, w.Body.String())
	}
	if w := upload(h, "river.csv", string(river), ""); w.Code != 409 {
		t.Errorf("same file again: %d", w.Code)
	}
	if w := upload(h, "mystery.csv", "when,what,how much\n2024-01-01,thing,1\n", ""); w.Code != 422 {
		t.Errorf("unknown format: %d %s", w.Code, w.Body.String())
	}
}

func TestFulcrumStatusNotSetUp(t *testing.T) {
	_, h := newTest(t)
	var st struct {
		OK    bool
		Error string
	}
	json.Unmarshal(call(h, "GET", "/api/fulcrum/status", "").Body.Bytes(), &st)
	if st.OK || !strings.Contains(st.Error, "address first") {
		t.Errorf("%+v", st)
	}
}

// The demo history is large and exercises every path: it must load, trace
// with no coins of unknown origin, and give the flow view plenty to draw.
func TestDemoReport(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	if err := demo.Seed(st, 1); err == nil {
		t.Fatal("seeded a database that already had data")
	}
	var rep Report
	w := call(h, "GET", "/api/report", "")
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(w.Code, err)
	}
	kinds := map[string]int{}
	for _, g := range rep.Graph {
		kinds[g.Kind]++
	}
	t.Logf("graph %d events %v, %d disposals, %d unmatched, %d warnings", len(rep.Graph), kinds, len(rep.Disposals), len(rep.Unmatched), len(rep.Warnings))
	if len(rep.Graph) < 500 || kinds["transfer"] < 50 || kinds["consolidation"] == 0 || len(rep.Unmatched) == 0 {
		t.Errorf("too small: %d events %v, %d unmatched", len(rep.Graph), kinds, len(rep.Unmatched))
	}
	for _, msg := range rep.Warnings {
		if strings.Contains(msg, "no known basis") || strings.Contains(msg, "coinjoin") {
			t.Errorf("warning: %s", msg)
		}
	}
	// Every wallet's traced balance matches the chain once the unlabeled
	// items still waiting in Review are accounted for.
	pending := map[string]int64{}
	for _, it := range rep.Unmatched {
		if it.Dir == "out" {
			pending[it.Account] += it.Sats + it.FeeSats
		} else {
			pending[it.Account] -= it.Sats
		}
	}
	for _, a := range rep.Accounts {
		if a.ChainSats != nil && *a.ChainSats+pending[a.Name] != a.Sats {
			t.Errorf("%s: traced %d, on chain %d, pending review %d", a.Name, a.Sats, *a.ChainSats, pending[a.Name])
		}
	}
}

func TestHistoryAndFreshness(t *testing.T) {
	st, h := newTest(t)
	if err := demo.Seed(st, 1); err != nil {
		t.Fatal(err)
	}
	var rep Report
	json.Unmarshal(call(h, "GET", "/api/report", "").Body.Bytes(), &rep)
	if len(rep.History) < 300 {
		t.Fatalf("%d history points", len(rep.History))
	}
	if last := rep.History[len(rep.History)-1]; last.Sats != rep.Holdings {
		t.Errorf("history ends at %d sats, holdings %d", last.Sats, rep.Holdings)
	}
	var fresh []struct {
		Account string
		Last    string
		Days    int
	}
	json.Unmarshal(call(h, "GET", "/api/imports/freshness", "").Body.Bytes(), &fresh)
	if len(fresh) != 3 {
		t.Errorf("%+v", fresh)
	}
}
