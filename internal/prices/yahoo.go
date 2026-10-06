package prices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Yahoo uses Yahoo Finance's public chart endpoint for BTC-USD. History
// starts 2014-09-17. The endpoint is unofficial and may change.
type Yahoo struct{ BaseURL string }

func init() { Register(Yahoo{}) }

func (Yahoo) ID() string   { return "yahoo" }
func (Yahoo) Name() string { return "Yahoo Finance (BTC-USD, from Sep 2014)" }
func (y Yahoo) Host() string {
	return "query1.finance.yahoo.com"
}

func (y Yahoo) Fetch(ctx context.Context, from, to time.Time) (map[string]int64, error) {
	base := y.BaseURL
	if base == "" {
		base = "https://query1.finance.yahoo.com"
	}
	url := fmt.Sprintf("%s/v8/finance/chart/BTC-USD?period1=%d&period2=%d&interval=1d&events=history", base, from.Unix(), to.Unix())
	resp, err := get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Close []*float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
			Error *struct {
				Code        string `json:"code"`
				Description string `json:"description"`
			} `json:"error"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("yahoo: %w", err)
	}
	if body.Chart.Error != nil {
		return nil, fmt.Errorf("yahoo: %s", body.Chart.Error.Description)
	}
	if len(body.Chart.Result) == 0 || len(body.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, errors.New("yahoo: empty response")
	}
	r := body.Chart.Result[0]
	closes := r.Indicators.Quote[0].Close
	out := map[string]int64{}
	for i, ts := range r.Timestamp {
		if i < len(closes) && closes[i] != nil && *closes[i] > 0 {
			out[time.Unix(ts, 0).UTC().Format("2006-01-02")] = floatToCents(*closes[i])
		}
	}
	// Today's bar is still moving; keep only completed days.
	delete(out, time.Now().UTC().Format("2006-01-02"))
	return out, nil
}
