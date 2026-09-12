package stockbit

import (
	"context"
	"encoding/json"
	"fmt"
	"indonesia-stock/internal/domain/orderbook"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL        string
	token          string
	http           *http.Client
	OnAuthRejected func() string
}

func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = "https://api.stockbit.com"
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Get(ctx context.Context, symbol string) (*orderbook.Orderbook, error) {
	return c.GetOrderbook(ctx, symbol)
}

func (c *Client) GetOrderbook(ctx context.Context, symbol string) (*orderbook.Orderbook, error) {
	symbol = strings.ToUpper(symbol)
	endpoints := []string{
		fmt.Sprintf("https://exodus.stockbit.com/company-price-feed/v2/orderbook/companies/%s", symbol),
		fmt.Sprintf("%s/v2.2/orderbook/%s", c.baseURL, symbol),
	}
	var lastErr error
	for i, url := range endpoints {
		ob, status, err := c.fetch(ctx, url, symbol)
		if err == nil {
			return ob, nil
		}
		// auth rejection: explicit 401/403, OR Stockbit's sneaky 200 +
		// InvalidParameter "Silahkan update aplikasi" (token rejected)
		authRejected := status == http.StatusUnauthorized || status == http.StatusForbidden ||
			strings.Contains(err.Error(), "Silahkan update aplikasi")
		if i == 0 && authRejected && c.OnAuthRejected != nil {
			if fresh := c.OnAuthRejected(); fresh != "" {
				c.token = fresh
				if ob2, _, err2 := c.fetch(ctx, url, symbol); err2 == nil {
					return ob2, nil
				} else {
					lastErr = err2
					continue
				}
			}
		}
		lastErr = err
	}
	hint := "save tokens from browser login via 'indostock auth save --token <JWT> --refresh <JWT> (or --stdin), then indostock auth status --json to verify.' (ADR-0009: server logins trigger OTP and fail; browser is source of truth) ; untuk test tanpa token gunakan --mock"
	return nil, fmt.Errorf("stockbit orderbook failed for %s: %w (%s)", symbol, lastErr, hint)
}

func (c *Client) fetch(ctx context.Context, url, symbol string) (*orderbook.Orderbook, int, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,id;q=0.8")
	req.Header.Set("Origin", "https://stockbit.com")
	req.Header.Set("Referer", "https://stockbit.com/")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Cookie", "access_token="+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		n := len(body)
		if n > 500 {
			n = 500
		}
		return nil, resp.StatusCode, fmt.Errorf("%d %s url=%s", resp.StatusCode, string(body[:n]), url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	parsed, err := parseStockbitBody(body, symbol)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse: %w body=%.500s", err, string(body))
	}
	parsed.Timestamp = time.Now()
	parsed.Source = "stockbit"
	parsed.Symbol = symbol
	return parsed, resp.StatusCode, nil
}

func parseStockbitBody(body []byte, symbol string) (*orderbook.Orderbook, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	out := &orderbook.Orderbook{Symbol: symbol}
	bidsRaw := findDeep(raw, []string{"bids", "bid"})
	asksRaw := findDeep(raw, []string{"asks", "ask", "offer", "offers"})
	if bidsRaw == nil && asksRaw == nil {
		return nil, fmt.Errorf("unknown orderbook shape, no bids/asks found")
	}
	if bidsRaw != nil {
		out.Bids = parseLevels(bidsRaw)
	}
	if asksRaw != nil {
		out.Asks = parseLevels(asksRaw)
	}
	if len(out.Bids) == 0 && len(out.Asks) == 0 {
		return nil, fmt.Errorf("bids/asks empty after parse bids=%v asks=%v", bidsRaw, asksRaw)
	}
	return out, nil
}

func findDeep(m map[string]any, keys []string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if found := findDeep(sub, keys); found != nil {
				return found
			}
		}
		if arr, ok := v.([]any); ok {
			for _, el := range arr {
				if sub, ok := el.(map[string]any); ok {
					if found := findDeep(sub, keys); found != nil {
						return found
					}
				}
			}
		}
	}
	return nil
}

// levelFromPair parses a [price, lot] array-shaped orderbook entry.
func levelFromPair(v []any) (orderbook.Level, bool) {
	if len(v) < 2 {
		return orderbook.Level{}, false
	}
	price := toFloat(v[0])
	lot := toLot(v[1])
	if price <= 0 {
		return orderbook.Level{}, false
	}
	return orderbook.Level{Price: price, Lot: lot}, true
}

// levelFromMap parses an object-shaped orderbook entry (several key styles).
func levelFromMap(v map[string]any) (orderbook.Level, bool) {
	price := toFloat(firstOf(v, []string{"price", "p", "harga", "Price"}))
	lot := toLot(firstOf(v, []string{"lot", "quantity", "qty", "volume", "vol", "q", "Lot", "remaining"}))
	orders := int(toFloat(firstOf(v, []string{"orders", "count", "n"})))
	if price == 0 {
		return orderbook.Level{}, false
	}
	if lot == 0 {
		lot = 1
	}
	return orderbook.Level{Price: price, Lot: lot, Orders: orders}, true
}

func parseLevels(raw any) []orderbook.Level {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var levels []orderbook.Level
	for _, item := range arr {
		switch v := item.(type) {
		case []any:
			if lvl, ok := levelFromPair(v); ok {
				levels = append(levels, lvl)
			}
		case map[string]any:
			if lvl, ok := levelFromMap(v); ok {
				levels = append(levels, lvl)
			}
		case float64:
			levels = append(levels, orderbook.Level{Price: v, Lot: 1})
		}
	}
	return levels
}

func firstOf(m map[string]any, keys []string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(x, ",", ""))
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
}

func toLot(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(x, ",", ""))
		f, _ := strconv.ParseFloat(s, 64)
		return int64(f)
	case json.Number:
		i, _ := x.Int64()
		return i
	default:
		return 0
	}
}

func (c *Client) Stream(ctx context.Context, symbol string, intervalSeconds int) (<-chan orderbook.Orderbook, error) {
	if intervalSeconds <= 0 {
		intervalSeconds = 2
	}
	ch := make(chan orderbook.Orderbook)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ob, err := c.GetOrderbook(ctx, symbol)
				if err != nil {
					continue
				}
				select {
				case ch <- *ob:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}
