// Package stockanalysis fetches fundamental data for IDX-listed equities
// from stockanalysis.com (data by S&P Global Market Intelligence).
//
// Mechanism (documented by github.com/haskaomni/stockanalysis): the site is a
// SvelteKit SPA; page data is available as JSON via the __data.json endpoint
// in SvelteKit "devalue" encoding. Public pages need NO auth cookies
// (verified 2026-09-12 from the production server).
//
// Devalue format (simplified): nodes[-1].data is an array; element 0 is the
// root object; every integer inside a container is a reference to
// data[thatIndex]. We resolve refs recursively, then walk well-known
// section ids ({id,title,value} triples) into a flat metric map.
package stockanalysis

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	baseURL    = "https://stockanalysis.com"
	defaultUA  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124 Safari/537.36"
	httpJitter = "10s"
)

// Fundamentals is the flattened metric set for one symbol.
type Fundamentals struct {
	Symbol     string `json:"symbol"`
	Name       string `json:"name,omitempty"`
	MarketCap  string `json:"market_cap,omitempty"`
	PE         string `json:"pe,omitempty"`
	PEForward  string `json:"pe_forward,omitempty"`
	PS         string `json:"ps,omitempty"`
	PBV        string `json:"pbv,omitempty"`
	PEG        string `json:"peg,omitempty"`
	ROE        string `json:"roe,omitempty"`
	ROA        string `json:"roa,omitempty"`
	DebtEquity string `json:"debt_equity,omitempty"`
	NetCash    string `json:"net_cash,omitempty"`
	BookPerSh  string `json:"book_value_per_share,omitempty"`
	EPS        string `json:"eps_ttm,omitempty"`
	Revenue    string `json:"revenue_ttm,omitempty"`
	NetIncome  string `json:"net_income_ttm,omitempty"`
	ProfMargin string `json:"profit_margin,omitempty"`
	OpMargin   string `json:"operating_margin,omitempty"`
	DivPerSh   string `json:"dividend_per_share,omitempty"`
	DivYield   string `json:"dividend_yield,omitempty"`
	Payout     string `json:"payout_ratio,omitempty"`
	Beta       string `json:"beta,omitempty"`
	Consensus  string `json:"analyst_consensus,omitempty"`
	PriceTP    string `json:"analyst_price_target,omitempty"`
	UpsidePct  string `json:"analyst_upside_pct,omitempty"`
	AsOf       string `json:"as_of"`
	Source     string `json:"source"`
}

// Client fetches and parses stockanalysis.com __data.json payloads.
type Client struct {
	HTTP *http.Client
	UA   string
}

// New returns a Client with sane timeouts.
func New() *Client {
	t := 15 * time.Second
	return &Client{HTTP: &http.Client{Timeout: t}, UA: defaultUA}
}

// FetchStatistics pulls the /quote/idx/{SYM}/statistics/ page data.
func (c *Client) FetchStatistics(symbol string) (*Fundamentals, error) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return nil, fmt.Errorf("empty symbol")
	}
	url := fmt.Sprintf("%s/quote/idx/%s/statistics/__data.json?x-sveltekit-trailing-slash=1", baseURL, sym)
	body, err := c.get(url)
	if err != nil {
		return nil, err
	}
	f, err := parseStatistics(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", sym, err)
	}
	f.Symbol = sym
	f.Source = "stockanalysis.com (S&P Global)"
	if f.AsOf == "" {
		f.AsOf = time.Now().UTC().Format("2006-01-02")
	}
	return f, nil
}

func (c *Client) get(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UA)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
}

// ---- devalue parsing ----

// payload is the minimal SvelteKit __data.json envelope.
type payload struct {
	Nodes []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"nodes"`
}

// resolve dereferences the devalue integer-ref encoding.
// data[0] is the root; any integer appearing as an object value / array
// element is a pointer to data[n]. Bare integers that are genuine numbers
// are rare in this payload (statistics page has none besides timestamps
// stored as strings), so the heuristic is safe here.
func resolve(data []json.RawMessage) (map[string]any, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty devalue payload")
	}
	var rootRaw any
	if err := json.Unmarshal(data[0], &rootRaw); err != nil {
		return nil, err
	}
	seen := 0
	var walk func(v any, depth int) any
	walk = func(v any, depth int) any {
		if depth > 32 || seen > 200000 {
			return nil
		}
		switch tv := v.(type) {
		case float64:
			idx := int(tv)
			if idx >= 0 && idx < len(data) && float64(idx) == tv {
				seen++
				var nxt any
				if err := json.Unmarshal(data[idx], &nxt); err != nil {
					return nil
				}
				return walk(nxt, depth+1)
			}
			return tv
		case map[string]any:
			out := make(map[string]any, len(tv))
			for k, vv := range tv {
				out[k] = walk(vv, depth+1)
			}
			return out
		case []any:
			out := make([]any, len(tv))
			for i, vv := range tv {
				out[i] = walk(vv, depth+1)
			}
			return out
		default:
			return v
		}
	}
	root, ok := walk(rootRaw, 0).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("root is not an object")
	}
	return root, nil
}

func parseStatistics(body []byte) (*Fundamentals, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if len(p.Nodes) == 0 {
		return nil, fmt.Errorf("no sveltekit nodes")
	}
	// find the last data node (node 0 is layout "skip"/session, page data last)
	var dataArr []json.RawMessage
	for i := len(p.Nodes) - 1; i >= 0; i-- {
		if p.Nodes[i].Type == "data" && len(p.Nodes[i].Data) > 0 {
			if err := json.Unmarshal(p.Nodes[i].Data, &dataArr); err == nil && len(dataArr) > 0 {
				break
			}
		}
	}
	if len(dataArr) == 0 {
		return nil, fmt.Errorf("no data array in sveltekit nodes")
	}
	root, err := resolve(dataArr)
	if err != nil {
		return nil, err
	}

	f := &Fundamentals{}
	// sections: map[sectionName] -> {"text":..., "data":[{"id","title","value","hover"}]}
	collect := func(section string, fn func(id, value, hover string)) {
		sec, ok := root[section].(map[string]any)
		if !ok {
			return
		}
		items, ok := sec["data"].([]any)
		if !ok {
			return
		}
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			val, _ := m["value"].(string)
			hov, _ := m["hover"].(string)
			fn(id, val, hov)
		}
	}
	// prefer hover (raw number) over display value when both exist
	setFrom := func(dst *string, val, hover string) {
		if hover != "" && hover != "n/a" {
			*dst = hover
		} else if val != "" && val != "n/a" {
			*dst = val
		}
	}

	collect("ratios", func(id, val, hov string) {
		switch id {
		case "pe":
			setFrom(&f.PE, val, hov)
		case "peForward":
			setFrom(&f.PEForward, val, hov)
		case "ps":
			setFrom(&f.PS, val, hov)
		case "pb":
			setFrom(&f.PBV, val, hov)
		case "pegRatio":
			setFrom(&f.PEG, val, hov)
		}
	})
	collect("valuation", func(id, val, hov string) {
		switch id {
		case "marketcap":
			setFrom(&f.MarketCap, val, hov)
		}
	})
	collect("financialEfficiency", func(id, val, hov string) {
		switch id {
		case "roe":
			setFrom(&f.ROE, val, hov)
		case "roa":
			setFrom(&f.ROA, val, hov)
		}
	})
	collect("financialPosition", func(id, val, hov string) {
		switch id {
		case "debtEquity":
			setFrom(&f.DebtEquity, val, hov)
		}
	})
	collect("balanceSheet", func(id, val, hov string) {
		switch id {
		case "bvps":
			setFrom(&f.BookPerSh, val, hov)
		case "netcash":
			setFrom(&f.NetCash, val, hov)
		}
	})
	collect("incomeStatement", func(id, val, hov string) {
		switch id {
		case "eps":
			setFrom(&f.EPS, val, hov)
		case "revenue":
			setFrom(&f.Revenue, val, hov)
		case "netIncome":
			setFrom(&f.NetIncome, val, hov)
		}
	})
	collect("margins", func(id, val, hov string) {
		switch id {
		case "operatingMargin":
			setFrom(&f.OpMargin, val, hov)
		case "profitMargin":
			setFrom(&f.ProfMargin, val, hov)
		}
	})
	collect("dividends", func(id, val, hov string) {
		switch id {
		case "dps", "dividendPerShare":
			setFrom(&f.DivPerSh, val, hov)
		case "dividendYield":
			setFrom(&f.DivYield, val, hov)
		case "payoutRatio":
			setFrom(&f.Payout, val, hov)
		}
	})
	collect("stockPrice", func(id, val, hov string) {
		if id == "beta" {
			setFrom(&f.Beta, val, hov)
		}
	})
	collect("analystForecasts", func(id, val, hov string) {
		switch id {
		case "analystRatings", "consensus":
			f.Consensus = val
		case "priceTarget":
			setFrom(&f.PriceTP, val, hov)
		case "priceTargetChange", "upside":
			setFrom(&f.UpsidePct, val, hov)
		}
	})

	if f.PE == "" && f.PBV == "" && f.ROE == "" {
		return nil, fmt.Errorf("payload resolved but contains no valuation metrics (page shape changed?)")
	}
	return f, nil
}

// Num parses "13.41" / "21.82%" / "776,974,076,972,500" → float64 (0 if n/a).
func Num(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "%")
	s = strings.ReplaceAll(s, ",", "")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
