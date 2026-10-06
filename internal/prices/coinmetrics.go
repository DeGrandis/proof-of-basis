package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// CoinMetrics uses the free Coin Metrics community API (no key). It has a
// daily reference rate back to July 2010, which covers early coins that
// Yahoo does not.
type CoinMetrics struct{ BaseURL string }

func init() { Register(CoinMetrics{}) }

func (CoinMetrics) ID() string   { return "coinmetrics" }
func (CoinMetrics) Name() string { return "Coin Metrics community API (from Jul 2010)" }
func (CoinMetrics) Host() string { return "community-api.coinmetrics.io" }

func (c CoinMetrics) Fetch(ctx context.Context, from, to time.Time) (map[string]int64, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://community-api.coinmetrics.io"
	}
	url := fmt.Sprintf("%s/v4/timeseries/asset-metrics?assets=btc&metrics=PriceUSD&frequency=1d&page_size=10000&start_time=%s&end_time=%s",
		base, from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"))
	out := map[string]int64{}
	for page := 0; url != "" && page < 20; page++ {
		resp, err := get(ctx, url)
		if err != nil {
			return nil, err
		}
		var body struct {
			Data []struct {
				Time     string `json:"time"`
				PriceUSD string `json:"PriceUSD"`
			} `json:"data"`
			Next string `json:"next_page_url"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("coinmetrics: %w", err)
		}
		for _, d := range body.Data {
			if len(d.Time) < 10 {
				continue
			}
			if c, err := dollarsToCents(d.PriceUSD); err == nil && c > 0 {
				out[d.Time[:10]] = c
			}
		}
		url = body.Next
	}
	return out, nil
}
