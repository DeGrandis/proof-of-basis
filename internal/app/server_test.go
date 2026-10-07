package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
