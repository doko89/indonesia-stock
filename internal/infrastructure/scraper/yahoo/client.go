package yahoo

import (
	"context"
	"encoding/json"
	"fmt"
	"indonesia-stock/internal/domain/stock"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "https://query1.finance.yahoo.com"
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) GetQuote(ctx context.Context, symbol string) (*stock.Quote, error) {
	ticker := fmt.Sprintf("%s.JK", symbol)
	url := fmt.Sprintf("%s/v8/finance/chart/%s?interval=1d&range=2d", c.baseURL, ticker)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("yahoo %d: %s", resp.StatusCode, string(body))
	}
	var raw chartResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if len(raw.Chart.Result) == 0 {
		return nil, fmt.Errorf("no data for %s", symbol)
	}
	r := raw.Chart.Result[0]
	meta := r.Meta
	q := r.Indicators.Quote[0]
	n := len(q.Close)
	if n == 0 {
		return nil, fmt.Errorf("empty quote")
	}
	idx := n - 1
	closePrice := q.Close[idx]
	openPrice := q.Open[idx]
	highPrice := q.High[idx]
	lowPrice := q.Low[idx]
	vol := int64(0)
	if idx < len(q.Volume) && q.Volume[idx] != nil {
		vol = int64(*q.Volume[idx])
	}
	prevClose := meta.PreviousClose
	if prevClose == 0 && n > 1 {
		prevClose = q.Close[n-2]
	}
	change := closePrice - prevClose
	var pct float64
	if prevClose != 0 {
		pct = change / prevClose * 100
	}
	ts := time.Now()
	if idx < len(r.Timestamp) {
		ts = time.Unix(r.Timestamp[idx], 0)
	}
	return &stock.Quote{
		Symbol:    symbol,
		Price:     closePrice,
		Open:      openPrice,
		High:      highPrice,
		Low:       lowPrice,
		Close:     closePrice,
		PrevClose: prevClose,
		Volume:    vol,
		Change:    change,
		ChangePct: pct,
		Timestamp: ts,
		Source:    "yahoo",
	}, nil
}

func (c *Client) GetHistory(ctx context.Context, symbol, interval, rangeStr string) ([]stock.Candle, error) {
	if interval == "" {
		interval = "1d"
	}
	if rangeStr == "" {
		rangeStr = "1mo"
	}
	ticker := fmt.Sprintf("%s.JK", symbol)
	url := fmt.Sprintf("%s/v8/finance/chart/%s?interval=%s&range=%s", c.baseURL, ticker, interval, rangeStr)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("yahoo %d: %s", resp.StatusCode, string(body))
	}
	var raw chartResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if len(raw.Chart.Result) == 0 {
		return nil, fmt.Errorf("no data")
	}
	r := raw.Chart.Result[0]
	q := r.Indicators.Quote[0]
	var out []stock.Candle
	for i, ts := range r.Timestamp {
		if i >= len(q.Close) {
			break
		}
		if q.Close[i] == 0 {
			continue
		}
		vol := int64(0)
		if i < len(q.Volume) && q.Volume[i] != nil {
			vol = int64(*q.Volume[i])
		}
		out = append(out, stock.Candle{
			Timestamp: time.Unix(ts, 0),
			Open:      q.Open[i],
			High:      q.High[i],
			Low:       q.Low[i],
			Close:     q.Close[i],
			Volume:    vol,
		})
	}
	return out, nil
}

type chartResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				PreviousClose      float64 `json:"previousClose"`
				RegularMarketPrice float64 `json:"regularMarketPrice"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []float64 `json:"open"`
					High   []float64 `json:"high"`
					Low    []float64 `json:"low"`
					Close  []float64 `json:"close"`
					Volume []*int64  `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
	} `json:"chart"`
}
