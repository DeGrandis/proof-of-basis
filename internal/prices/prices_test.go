package prices

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Fake servers return invented prices; no network is used.

func TestYahoo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/finance/chart/BTC-USD" || r.Header.Get("User-Agent") == "" {
			http.Error(w, "bad request", 400)
			return
		}
		w.Write([]byte(`{"chart":{"result":[{"timestamp":[1704067200,1704153600,1704240000],
		"indicators":{"quote":[{"close":[42000.126,null,43000.5]}]}}],"error":null}}`))
	}))
	defer srv.Close()
	m, err := Yahoo{BaseURL: srv.URL}.Fetch(context.Background(), time.Unix(0, 0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["2024-01-01"] != 4200013 || m["2024-01-03"] != 4300050 {
		t.Errorf("%v", m)
	}
}

func TestCoinMetricsPaging(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Write([]byte(`{"data":[{"asset":"btc","time":"2010-07-18T00:00:00.000000000Z","PriceUSD":"0.0858"}],"next_page_url":"` + srv.URL + `/v4/timeseries/asset-metrics?page=2"}`))
			return
		}
		w.Write([]byte(`{"data":[{"asset":"btc","time":"2010-07-19T00:00:00.000000000Z","PriceUSD":"0.0808"}]}`))
	}))
	defer srv.Close()
	m, err := CoinMetrics{BaseURL: srv.URL}.Fetch(context.Background(), time.Unix(0, 0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m["2010-07-18"] != 9 || m["2010-07-19"] != 8 {
		t.Errorf("%v", m)
	}
}

func TestCSVRoundTrip(t *testing.T) {
	in := map[string]int64{"2024-01-02": 4512345, "2024-01-01": 4200000}
	var b bytes.Buffer
	WriteCSV(&b, in)
	if b.String() != "date,close_usd\n2024-01-01,42000.00\n2024-01-02,45123.45\n" {
		t.Errorf("%q", b.String())
	}
	out, err := ReadCSV(&b)
	if err != nil || len(out) != 2 || out["2024-01-02"] != 4512345 {
		t.Errorf("%v %v", out, err)
	}
	if _, err := ReadCSV(bytes.NewBufferString("2024-01-01,abc\n")); err == nil {
		t.Error("bad price accepted")
	}
}

func TestBundledParses(t *testing.T) { _ = Bundled() }
