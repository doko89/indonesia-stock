package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	finapp "indonesia-stock/internal/application/financial"
	orderbookapp "indonesia-stock/internal/application/orderbook"
	"indonesia-stock/internal/application/quote"
	findomain "indonesia-stock/internal/domain/financial"
	"indonesia-stock/internal/domain/orderbook"
	"indonesia-stock/internal/domain/signal"
	"indonesia-stock/internal/infrastructure/scraper/idx"
	"indonesia-stock/internal/infrastructure/scraper/rti"
	"indonesia-stock/internal/infrastructure/scraper/stockbit"
	"indonesia-stock/internal/infrastructure/scraper/yahoo"
	"indonesia-stock/internal/infrastructure/cache"
	"indonesia-stock/pkg/auth"
	"indonesia-stock/pkg/config"
	"bufio"
	"github.com/robfig/cron/v3"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func Run(args []string) int {
	cfg := config.Load()
	if len(args) < 2 {
		printHelp()
		return 0
	}
	cmd := strings.ToLower(args[1])
	rest := args[2:]

	switch cmd {
	case "help", "--help", "-h":
		printHelp()
		return 0
	case "version", "--version":
		fmt.Println("indostock v0.1.0 (go-ddd + orderbook)")
		return 0
	case "quote":
		return runQuote(cfg, rest)
	case "history", "candle", "candles":
		return runHistory(cfg, rest)
	case "orderbook", "ob", "book":
		return runOrderbook(cfg, rest)
	case "financial", "fundamental", "report":
		return runFinancial(cfg, rest)
	case "idx", "xbrl", "idx_xbrl":
		return runIDX(cfg, rest)
	case "whale":
		return runWhale(cfg, rest)
	case "regime":
		return runRegime(cfg, rest)
	case "signal":
		return runSignal(cfg, rest)
	case "serve", "server", "http":
		return runServe(cfg, rest)
	case "snapshot", "analyze":
		return runSnapshot(cfg, rest)
	case "auth", "token", "login":
		return runAuth(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printHelp()
		return 1
	}
}

func printHelp() {
	fmt.Print(`indostock - Indonesia Stock CLI for AI trading agents (DDD + Orderbook)

Usage:
  indostock <command> [options] <symbol>

Commands:
  quote <SYMBOL>                 Get last quote (Yahoo Finance)       e.g. indostock quote BBCA --json
  history <SYMBOL>               Get OHLCV candles                    e.g. indostock history BBCA --interval 1d --range 1mo --json
  orderbook <SYMBOL>             Get orderbook depth (Stockbit)       e.g. indostock orderbook BBCA --depth 10 --json --mock
  financial <SYMBOL>             Get financial report (RTI)           e.g. indostock financial BBCA --period annual --type income_statement --json --mock
  idx <SYMBOL>                   Get IDX InlineXBRL report            e.g. indostock idx BBCA --year 2024 --quarter 1 --json --mock
  whale <SYMBOL>                 Detect whale activity (Z-Score)      e.g. indostock whale BBCA --z 3 --window 60 --mock
  regime <SYMBOL>                Detect market regime (ATR/EMA)       e.g. indostock regime BBCA --mock
  signal <SYMBOL>                Generate trading signal (whale+regime+orderflow) e.g. indostock signal BBCA --mock
  auth status|refresh|save       Manage Stockbit token auto-rotate  e.g. indostock auth status --json
  snapshot <SYMBOL>              AI-friendly snapshot: quote + orderbook + imbalance + spread in one JSON

Global flags (per command):
  --json          Output JSON to stdout (default true for AI agents, use --json=false for table)
  --compact       Compact JSON (one line, good for tool calling)
  --csv           Output CSV (for history)
  --watch         Stream continuously (orderbook)
  --interval N    Poll interval seconds (default 2) or candle interval (1m,5m,1d)
  --range R       Yahoo range: 1d,5d,1mo,3mo,1y,5y (history)
  --depth N       Orderbook depth limit (default 10)
  --period P      annual|quarterly (financial, default annual)
  --type T        income_statement|balance_sheet|cash_flow|general_info (financial)
  --year Y        Year for IDX XBRL (default current year)
  --quarter Q     Quarter 1-4 for IDX XBRL (default 1)
  --z Z           Z-Score threshold for whale (default 3.0)
  --window W      Rolling window size for whale baseline (default 60)
  --mock          Use synthetic/mock data (offline testing for AI agents, no network/token needed)
  --debug         Dump raw orderbook JSON to stderr

Env:
  STOCKBIT_TOKEN   Bearer token for Stockbit orderbook (if not set, tries public endpoint)
  STOCKBIT_BASE_URL, YAHOO_BASE_URL, RTI_BASE_URL, IDX_BASE_URL

Examples for AI agent:
  indostock quote BBCA --json
  indostock orderbook BBCA --depth 5 --json --mock   # test without token
  indostock orderbook BBCA --depth 5 --json         # live (needs STOCKBIT_TOKEN)
  indostock orderbook BBCA --watch --interval 2 --json --compact  # streaming NDJSON
  indostock snapshot BBCA --json  # one-shot comprehensive view
  indostock snapshot BBCA --json --mock  # offline snapshot
  indostock history BBCA --interval 1d --range 3mo --json > candles.json
  indostock financial BBCA --period annual --type balance_sheet --json --mock
  indostock idx BBCA --year 2024 --quarter 2 --json --mock
  indostock whale BBCA --z 3 --mock --compact
  indostock regime BBCA --mock --compact
  indostock signal BBCA --mock --compact

Output:
  Always JSON to stdout, logs to stderr, exit 0 on success. Suitable for exec tool calls.
`)
}

func outputJSON(v any, compact bool) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if !compact {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(v)
}

func parseFlags(rest []string, defaults map[string]string) (symbol string, flags map[string]string, compact, jsonOut, csvOut bool) {
	// manual flag parser: supports flags before or after SYMBOL (AI agents call both forms)
	jsonOut = true
	compact = false
	csvOut = false
	watchStr := "false"
	mockStr := "false"
	debugStr := "false"
	interval := defaults["interval"]
	rangeV := defaults["range"]
	depth := defaults["depth"]
	period := defaults["period"]
	typeV := defaults["type"]
	year := defaults["year"]
	quarter := defaults["quarter"]
	zVal := defaults["z"]
	windowVal := defaults["window"]
	portVal := defaults["port"]

	// use std flag for help detection but main parsing is manual
	fs := flag.NewFlagSet("cmd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_ = fs.Parse([]string{}) // dummy to keep import used

	args := []string{}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--json=false" || a == "--json=0":
			jsonOut = false
		case a == "--compact":
			compact = true
		case a == "--csv":
			csvOut = true
		case a == "--watch":
			watchStr = "true"
		case a == "--mock":
			mockStr = "true"
		case a == "--debug":
			debugStr = "true"
		case strings.HasPrefix(a, "--interval="):
			interval = strings.TrimPrefix(a, "--interval=")
		case a == "--interval" && i+1 < len(rest):
			i++
			interval = rest[i]
		case strings.HasPrefix(a, "--range="):
			rangeV = strings.TrimPrefix(a, "--range=")
		case a == "--range" && i+1 < len(rest):
			i++
			rangeV = rest[i]
		case strings.HasPrefix(a, "--depth="):
			depth = strings.TrimPrefix(a, "--depth=")
		case a == "--depth" && i+1 < len(rest):
			i++
			depth = rest[i]
		case strings.HasPrefix(a, "--period="):
			period = strings.TrimPrefix(a, "--period=")
		case a == "--period" && i+1 < len(rest):
			i++
			period = rest[i]
		case strings.HasPrefix(a, "--type="):
			typeV = strings.TrimPrefix(a, "--type=")
		case a == "--type" && i+1 < len(rest):
			i++
			typeV = rest[i]
		case strings.HasPrefix(a, "--year="):
			year = strings.TrimPrefix(a, "--year=")
		case a == "--year" && i+1 < len(rest):
			i++
			year = rest[i]
		case strings.HasPrefix(a, "--quarter="):
			quarter = strings.TrimPrefix(a, "--quarter=")
		case a == "--quarter" && i+1 < len(rest):
			i++
			quarter = rest[i]
		case strings.HasPrefix(a, "--z="):
			zVal = strings.TrimPrefix(a, "--z=")
		case a == "--z" && i+1 < len(rest):
			i++
			zVal = rest[i]
		case strings.HasPrefix(a, "--window="):
			windowVal = strings.TrimPrefix(a, "--window=")
		case a == "--window" && i+1 < len(rest):
			i++
			windowVal = rest[i]
		case strings.HasPrefix(a, "--port="):
			portVal = strings.TrimPrefix(a, "--port=")
		case a == "--port" && i+1 < len(rest):
			i++
			portVal = rest[i]
		case strings.HasPrefix(a, "--"):
			fmt.Fprintf(os.Stderr, "unknown flag %s\n", a)
		default:
			if symbol == "" && !strings.HasPrefix(a, "-") {
				symbol = strings.ToUpper(a)
			} else {
				args = append(args, a)
			}
		}
	}
	_ = args
	_ = fs
	flags = map[string]string{
		"interval": interval,
		"range":    rangeV,
		"depth":    depth,
		"period":   period,
		"type":     typeV,
		"year":     year,
		"quarter":  quarter,
		"z":        zVal,
		"window":   windowVal,
		"port":     portVal,
		"watch":    watchStr,
		"mock":     mockStr,
		"debug":    debugStr,
	}
	return symbol, flags, compact, jsonOut, csvOut
}

func runQuote(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"interval": "", "range": "", "depth": "", "period": "", "type": ""})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "quote: need SYMBOL, e.g. indostock quote BBCA")
		return 1
	}
	if flags["mock"] == "true" {
		outputJSON(mockQuote(symbol), compact)
		return 0
	}
	client := yahoo.New(cfg.YahooBaseURL)
	svc := quote.New(client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	q, err := svc.GetQuote(ctx, symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "quote error: %v\n", err)
		outputJSON(map[string]any{"error": err.Error(), "symbol": symbol, "hint": "gunakan --mock untuk test offline"}, compact)
		return 1
	}
	outputJSON(q, compact)
	return 0
}

func runHistory(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, csvOut := parseFlags(rest, map[string]string{"interval": "1d", "range": "1mo"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "history: need SYMBOL")
		return 1
	}
	interval := flags["interval"]
	rangeStr := flags["range"]
	if flags["mock"] == "true" {
		candles := mockHistory(symbol, interval, rangeStr)
		if csvOut {
			fmt.Println("timestamp,open,high,low,close,volume")
			for _, c := range candles {
				fmt.Printf("%s,%.2f,%.2f,%.2f,%.2f,%d\n", c["timestamp"], c["open"], c["high"], c["low"], c["close"], c["volume"])
			}
			return 0
		}
		outputJSON(map[string]any{"symbol": symbol, "interval": interval, "range": rangeStr, "candles": candles, "count": len(candles), "source": "mock"}, compact)
		return 0
	}
	client := yahoo.New(cfg.YahooBaseURL)
	svc := quote.New(client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	candles, err := svc.GetHistory(ctx, symbol, interval, rangeStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "history error: %v\n", err)
		outputJSON(map[string]any{"error": err.Error(), "hint": "gunakan --mock untuk test offline"}, compact)
		return 1
	}
	if csvOut {
		fmt.Println("timestamp,open,high,low,close,volume")
		for _, c := range candles {
			fmt.Printf("%s,%.2f,%.2f,%.2f,%.2f,%d\n", c.Timestamp.Format("2006-01-02"), c.Open, c.High, c.Low, c.Close, c.Volume)
		}
		return 0
	}
	outputJSON(map[string]any{"symbol": symbol, "interval": interval, "range": rangeStr, "candles": candles, "count": len(candles)}, compact)
	return 0
}

func runOrderbook(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"interval": "2", "depth": "10"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "orderbook: need SYMBOL, e.g. indostock orderbook BBCA --depth 10")
		return 1
	}
	depth := 10
	fmt.Sscan(flags["depth"], &depth)
	watch := flags["watch"] == "true"
	mock := flags["mock"] == "true"
	interval := 2
	fmt.Sscan(flags["interval"], &interval)

	if mock {
		if watch {
			ticker := time.NewTicker(time.Duration(interval) * time.Second)
			defer ticker.Stop()
			for {
				ob := mockOrderbook(symbol, depth)
				outputJSON(enrichOrderbook(ob), true)
				<-ticker.C
			}
		}
		ob := mockOrderbook(symbol, depth)
		outputJSON(enrichOrderbook(ob), compact)
		return 0
	}

	client := stockbit.New(cfg.StockbitBaseURL, cfg.StockbitToken)
	svc := orderbookapp.New(client)

	if watch {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := svc.Stream(ctx, symbol, interval)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
			return 1
		}
		for ob := range ch {
			if depth > 0 {
				if len(ob.Bids) > depth {
					ob.Bids = ob.Bids[:depth]
				}
				if len(ob.Asks) > depth {
					ob.Asks = ob.Asks[:depth]
				}
			}
			enriched := enrichOrderbook(&ob)
			outputJSON(enriched, true)
		}
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ob, err := svc.Get(ctx, symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orderbook error: %v\n", err)
		hint := "Set STOCKBIT_TOKEN (export STOCKBIT_TOKEN=xxx) atau gunakan --mock untuk test offline"
		if cfg.StockbitToken == "" {
			hint += " | Cara dapat token: POST https://api.stockbit.com/v2/login atau DevTools stockbit.com -> Network -> Authorization: Bearer ..."
		}
		outputJSON(map[string]any{"error": err.Error(), "symbol": symbol, "hint": hint, "endpoint": "https://exodus.stockbit.com/company-price-feed/v2/orderbook/companies/" + symbol}, compact)
		return 1
	}
	if depth > 0 {
		if len(ob.Bids) > depth {
			ob.Bids = ob.Bids[:depth]
		}
		if len(ob.Asks) > depth {
			ob.Asks = ob.Asks[:depth]
		}
	}
	enriched := enrichOrderbook(ob)
	outputJSON(enriched, compact)
	return 0
}

func runFinancial(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"period": "annual", "type": "income_statement"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "financial: need SYMBOL")
		return 1
	}
	period := flags["period"]
	typeStr := flags["type"]
	if period == "" {
		period = "annual"
	}
	if typeStr == "" {
		typeStr = "income_statement"
	}
	if typeStr == "general" || typeStr == "general_info" {
		client := rti.New(cfg.RTIBaseURL)
		svc := finapp.New(client)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		info, err := svc.GetGeneralInfo(ctx, symbol)
		if err != nil {
			fmt.Fprintf(os.Stderr, "financial general error: %v\n", err)
			outputJSON(map[string]any{"error": err.Error()}, compact)
			return 1
		}
		outputJSON(info, compact)
		return 0
	}
	client := rti.New(cfg.RTIBaseURL)
	svc := finapp.New(client)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report, err := svc.GetReport(ctx, symbol, findomain.Period(period), findomain.ReportType(typeStr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "financial error: %v\n", err)
		outputJSON(map[string]any{"error": err.Error()}, compact)
		return 1
	}
	outputJSON(report, compact)
	return 0
}

func runIDX(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"year": "", "quarter": "1"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "idx: need SYMBOL, e.g. indostock idx BBCA --year 2024 --quarter 1 --json --mock")
		return 1
	}
	year := time.Now().Year()
	if flags["year"] != "" {
		fmt.Sscan(flags["year"], &year)
	}
	quarter := 1
	if flags["quarter"] != "" {
		fmt.Sscan(flags["quarter"], &quarter)
	}
	if quarter < 1 {
		quarter = 1
	}
	if quarter > 4 {
		quarter = 4
	}
	client := idx.New(cfg.IDXBaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, err := client.GetXBRLReport(ctx, year, quarter, symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idx error: %v\n", err)
		outputJSON(map[string]any{"error": err.Error(), "symbol": symbol}, compact)
		return 1
	}
	outputJSON(rep, compact)
	return 0
}

func runSnapshot(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "snapshot: need SYMBOL, e.g. indostock snapshot BBCA --json")
		return 1
	}
	mock := flags["mock"] == "true"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	yClient := yahoo.New(cfg.YahooBaseURL)
	qSvc := quote.New(yClient)
	sClient := stockbit.New(cfg.StockbitBaseURL, cfg.StockbitToken)
	obSvc := orderbookapp.New(sClient)

	type result struct {
		Quote     any       `json:"quote"`
		Orderbook any       `json:"orderbook"`
		Analysis  any       `json:"analysis"`
		Symbol    string    `json:"symbol"`
		Timestamp time.Time `json:"timestamp"`
	}

	quoteCh := make(chan any, 1)
	obCh := make(chan any, 1)

	go func() {
		if mock {
			quoteCh <- mockQuote(symbol)
			return
		}
		q, err := qSvc.GetQuote(ctx, symbol)
		if err != nil {
			quoteCh <- map[string]any{"error": err.Error()}
			return
		}
		quoteCh <- q
	}()
	go func() {
		if mock {
			ob := mockOrderbook(symbol, 10)
			obCh <- enrichOrderbook(ob)
			return
		}
		ob, err := obSvc.Get(ctx, symbol)
		if err != nil {
			obCh <- map[string]any{"error": err.Error(), "hint": "orderbook needs STOCKBIT_TOKEN, gunakan --mock untuk test", "endpoint": "https://exodus.stockbit.com/company-price-feed/v2/orderbook/companies/" + symbol}
			return
		}
		if len(ob.Bids) > 10 {
			ob.Bids = ob.Bids[:10]
		}
		if len(ob.Asks) > 10 {
			ob.Asks = ob.Asks[:10]
		}
		obCh <- enrichOrderbook(ob)
	}()

	var qRes, obRes any
	select {
	case qRes = <-quoteCh:
	case <-ctx.Done():
		qRes = map[string]any{"error": "timeout"}
	}
	select {
	case obRes = <-obCh:
	case <-ctx.Done():
		obRes = map[string]any{"error": "timeout"}
	}

	analysis := buildAnalysis(qRes, obRes)

	out := result{
		Symbol:    symbol,
		Quote:     qRes,
		Orderbook: obRes,
		Analysis:  analysis,
		Timestamp: time.Now(),
	}
	outputJSON(out, compact)
	return 0
}

func enrichOrderbook(v any) any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	bidsRaw, _ := m["bids"].([]any)
	asksRaw, _ := m["asks"].([]any)
	var bestBid, bestAsk float64
	var bidVol, askVol int64
	totalBidVol := int64(0)
	totalAskVol := int64(0)
	for _, b := range bidsRaw {
		if mm, ok := b.(map[string]any); ok {
			if p, ok := mm["price"].(float64); ok && bestBid == 0 {
				bestBid = p
			}
			if l, ok := mm["lot"].(float64); ok {
				totalBidVol += int64(l)
			}
		}
	}
	for _, a := range asksRaw {
		if mm, ok := a.(map[string]any); ok {
			if p, ok := mm["price"].(float64); ok && bestAsk == 0 {
				bestAsk = p
			}
			if l, ok := mm["lot"].(float64); ok {
				totalAskVol += int64(l)
			}
		}
	}
	bidVol = totalBidVol
	askVol = totalAskVol
	spread := 0.0
	mid := 0.0
	if bestBid > 0 && bestAsk > 0 {
		spread = bestAsk - bestBid
		mid = (bestBid + bestAsk) / 2
	}
	total := bidVol + askVol
	ratio := 0.0
	if total > 0 {
		ratio = float64(bidVol-askVol) / float64(total)
	}
	m["best_bid"] = bestBid
	m["best_ask"] = bestAsk
	m["spread"] = spread
	m["mid_price"] = mid
	m["imbalance"] = map[string]any{"bid_volume": bidVol, "ask_volume": askVol, "ratio": ratio}
	return m
}

func buildAnalysis(quoteRes, obRes any) any {
	qMap := map[string]any{}
	b, _ := json.Marshal(quoteRes)
	_ = json.Unmarshal(b, &qMap)
	obMap := map[string]any{}
	b2, _ := json.Marshal(obRes)
	_ = json.Unmarshal(b2, &obMap)

	changePct := 0.0
	if v, ok := qMap["change_pct"].(float64); ok {
		changePct = v
	}
	ratio := 0.0
	if imb, ok := obMap["imbalance"].(map[string]any); ok {
		if r, ok := imb["ratio"].(float64); ok {
			ratio = r
		}
	}
	signal := "neutral"
	if changePct > 1 && ratio > 0.2 {
		signal = "bullish_pressure"
	} else if changePct < -1 && ratio < -0.2 {
		signal = "bearish_pressure"
	} else if ratio > 0.3 {
		signal = "bid_dominance"
	} else if ratio < -0.3 {
		signal = "ask_dominance"
	}
	return map[string]any{
		"signal":          signal,
		"change_pct":      changePct,
		"imbalance_ratio": ratio,
		"note":            "AI agent should combine this with history/financial before trading. Not financial advice.",
	}
}

func mockOrderbook(symbol string, depth int) *orderbook.Orderbook {
	if depth <= 0 {
		depth = 10
	}
	if depth > 20 {
		depth = 20
	}
	base := map[string]float64{"BBCA": 10125, "BBRI": 5400, "BMRI": 6800, "TLKM": 3100, "ASII": 5200, "GOTO": 85, "BRIS": 2900}
	mid := 10000.0
	if v, ok := base[symbol]; ok {
		mid = v
	} else {
		h := 0
		for _, c := range symbol {
			h = h*31 + int(c)
		}
		if h < 0 {
			h = -h
		}
		mid = 500 + float64(h%20000)
	}
	var bids, asks []orderbook.Level
	for i := 0; i < depth; i++ {
		bidPrice := mid - float64((i+1)*25) - float64(rand.Intn(5))
		askPrice := mid + float64((i+1)*25) + float64(rand.Intn(5))
		bids = append(bids, orderbook.Level{Price: bidPrice, Lot: int64(50 + rand.Intn(900))})
		asks = append(asks, orderbook.Level{Price: askPrice, Lot: int64(50 + rand.Intn(900))})
	}
	return &orderbook.Orderbook{
		Symbol:    symbol,
		Timestamp: time.Now(),
		Source:    "mock",
		Bids:      bids,
		Asks:      asks,
	}
}

func mockQuote(symbol string) map[string]any {
	base := map[string]float64{"BBCA": 10125, "BBRI": 5400, "BMRI": 6800, "TLKM": 3100, "ASII": 5200, "GOTO": 85}
	price := 5000.0
	if v, ok := base[symbol]; ok {
		price = v
	} else {
		h := 0
		for _, c := range symbol { h = h*31 + int(c) }
		if h < 0 { h = -h }
		price = 400 + float64(h%15000)
	}
	price = price * (0.98 + rand.Float64()*0.04)
	open := price * (0.99 + rand.Float64()*0.02)
	high := price * 1.01
	low := price * 0.99
	prev := price * (0.995 + rand.Float64()*0.01)
	change := price - prev
	pct := change / prev * 100
	return map[string]any{
		"symbol":     symbol,
		"price":      float64(int(price*100)) / 100,
		"open":       float64(int(open*100)) / 100,
		"high":       float64(int(high*100)) / 100,
		"low":        float64(int(low*100)) / 100,
		"close":      float64(int(price*100)) / 100,
		"prev_close": float64(int(prev*100)) / 100,
		"volume":     int64(1000000 + rand.Intn(9000000)),
		"change":     float64(int(change*100)) / 100,
		"change_pct": float64(int(pct*100)) / 100,
		"timestamp":  time.Now(),
		"source":     "mock",
	}
}

func mockHistory(symbol, interval, rangeStr string) []map[string]any {
	base := mockQuote(symbol)["price"].(float64)
	count := 30
	switch rangeStr {
	case "1d": count = 7
	case "5d": count = 5
	case "1mo": count = 22
	case "3mo": count = 60
	case "1y": count = 250
	case "5y": count = 60
	}
	if count > 120 { count = 120 }
	var out []map[string]any
	price := base
	now := time.Now()
	for i := count; i >= 0; i-- {
		price = price * (0.98 + rand.Float64()*0.04)
		ts := now.AddDate(0, 0, -i)
		out = append(out, map[string]any{
			"timestamp": ts.Format(time.RFC3339),
			"open":      float64(int(price*100)) / 100,
			"high":      float64(int(price*1.015*100)) / 100,
			"low":       float64(int(price*0.985*100)) / 100,
			"close":     float64(int(price*100)) / 100,
			"volume":    int64(500000 + rand.Intn(5000000)),
		})
	}
	return out
}

func runWhale(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"z": "3.0", "window": "60"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "whale: need SYMBOL, e.g. indostock whale BBCA --z 3 --mock")
		return 1
	}
	zThresh := 3.0
	fmt.Sscan(flags["z"], &zThresh)
	window := 60
	fmt.Sscan(flags["window"], &window)
	mock := flags["mock"] == "true"
	var candles []map[string]any
	var latestVol int64
	if mock {
		hist := mockHistory(symbol, "1d", "3mo")
		candles = hist
		if len(hist) > 0 {
			if v, ok := hist[len(hist)-1]["volume"].(int64); ok {
				latestVol = v
			}
		}
		if rand.Float64() < 0.15 {
			latestVol = latestVol * 6
		}
	} else {
		client := yahoo.New(cfg.YahooBaseURL)
		svc := quote.New(client)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cs, err := svc.GetHistory(ctx, symbol, "1d", "3mo")
		if err != nil {
			fmt.Fprintf(os.Stderr, "whale history error: %v\n", err)
			outputJSON(map[string]any{"error": err.Error(), "hint": "gunakan --mock"}, compact)
			return 1
		}
		for _, c := range cs {
			candles = append(candles, map[string]any{"volume": c.Volume, "close": c.Close})
		}
		if len(cs) > 0 {
			latestVol = cs[len(cs)-1].Volume
		}
	}
	if window > len(candles)-1 {
		window = len(candles) - 1
	}
	if window < 10 {
		window = 10
	}
	start := len(candles) - window - 1
	if start < 0 {
		start = 0
	}
	var vols []float64
	for i := start; i < len(candles)-1; i++ {
		if v, ok := candles[i]["volume"].(int64); ok {
			vols = append(vols, float64(v))
		} else if vf, ok := candles[i]["volume"].(float64); ok {
			vols = append(vols, vf)
		}
	}
	mean, std := meanStd(vols)
	z := 0.0
	relPct := 0.0
	if std > 0 {
		z = (float64(latestVol) - mean) / std
	}
	if mean > 0 {
		relPct = float64(latestVol) / mean * 100
	}
	isWhale := z >= zThresh || relPct >= 500
	confidence := 0.0
	reason := "no whale"
	if isWhale {
		confidence = 70 + (z-zThresh)*15
		if relPct > 500 {
			confidence += 5
		}
		if confidence > 99 {
			confidence = 99
		}
		reason = fmt.Sprintf("whale detected Z=%.2f RelVol=%.0f%%", z, relPct)
	}
	res := signal.WhaleResult{
		Symbol: symbol, Volume: latestVol, MeanVol: mean, StdDev: std, ZScore: z, RelVolPct: relPct, IsWhale: isWhale, Confidence: confidence, Reason: reason, Source: "mock", Timestamp: time.Now(),
	}
	if !mock {
		res.Source = "yahoo"
	}
	outputJSON(res, compact)
	return 0
}

func runRegime(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "regime: need SYMBOL, e.g. indostock regime BBCA --mock")
		return 1
	}
	mock := flags["mock"] == "true"
	var candles []map[string]any
	if mock {
		candles = mockHistory(symbol, "1d", "3mo")
	} else {
		client := yahoo.New(cfg.YahooBaseURL)
		svc := quote.New(client)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cs, err := svc.GetHistory(ctx, symbol, "1d", "3mo")
		if err != nil {
			fmt.Fprintf(os.Stderr, "regime history error: %v\n", err)
			outputJSON(map[string]any{"error": err.Error()}, compact)
			return 1
		}
		for _, c := range cs {
			candles = append(candles, map[string]any{"high": c.High, "low": c.Low, "close": c.Close})
		}
	}
	atr, atrPct := calcATR(candles, 14)
	emaSlope := calcEMASlope(candles, 20)
	regime := signal.RegimeRanging
	if atrPct > 2.0 {
		regime = signal.RegimeVolatile
	} else if math.Abs(emaSlope) > 0.5 {
		regime = signal.RegimeTrending
	}
	conf := 0.6
	if regime == signal.RegimeTrending {
		conf = 0.75
	} else if regime == signal.RegimeVolatile {
		conf = 0.45
	}
	src := "mock"
	if !mock {
		src = "yahoo"
	}
	res := signal.RegimeResult{Symbol: symbol, Regime: regime, ATR: atr, ATRPercent: atrPct, EMASlopePct: emaSlope, Confidence: conf, Source: src, Timestamp: time.Now()}
	outputJSON(res, compact)
	return 0
}

func runSignal(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"z": "3.0"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "signal: need SYMBOL, e.g. indostock signal BBCA --mock")
		return 1
	}
	mock := flags["mock"] == "true"
	// gather whale, regime, orderbook, quote
	// use internal helpers without extra network
	var whaleRes signal.WhaleResult
	var regimeRes signal.RegimeResult
	var imbalance float64
	var changePct float64
	if mock {
		// whale
		hist := mockHistory(symbol, "1d", "3mo")
		var vols []float64
		for i := len(hist) - 61; i < len(hist)-1; i++ {
			if i < 0 {
				continue
			}
			if v, ok := hist[i]["volume"].(int64); ok {
				vols = append(vols, float64(v))
			}
		}
		mean, std := meanStd(vols)
		latestVol := hist[len(hist)-1]["volume"].(int64)
		if rand.Float64() < 0.15 {
			latestVol *= 6
		}
		z := 0.0
		if std > 0 {
			z = (float64(latestVol) - mean) / std
		}
		relPct := 0.0
		if mean > 0 {
			relPct = float64(latestVol) / mean * 100
		}
		isWhale := z >= 3.0 || relPct >= 500
		whaleRes = signal.WhaleResult{Symbol: symbol, Volume: latestVol, MeanVol: mean, StdDev: std, ZScore: z, RelVolPct: relPct, IsWhale: isWhale}
		// regime
		atr, atrPct := calcATR(hist, 14)
		emaSlope := calcEMASlope(hist, 20)
		reg := signal.RegimeRanging
		if atrPct > 2.0 {
			reg = signal.RegimeVolatile
		} else if math.Abs(emaSlope) > 0.5 {
			reg = signal.RegimeTrending
		}
		regimeRes = signal.RegimeResult{Symbol: symbol, Regime: reg, ATR: atr, ATRPercent: atrPct, EMASlopePct: emaSlope}
		// orderbook imbalance
		ob := mockOrderbook(symbol, 10)
		imb := ob.Imbalance()
		imbalance = imb.Ratio
		q := mockQuote(symbol)
		changePct = q["change_pct"].(float64)
	} else {
		// live: fetch via services
		yClient := yahoo.New(cfg.YahooBaseURL)
		qSvc := quote.New(yClient)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cs, err := qSvc.GetHistory(ctx, symbol, "1d", "3mo")
		if err != nil {
			outputJSON(map[string]any{"error": err.Error()}, compact)
			return 1
		}
		var vols []float64
		for i := len(cs) - 61; i < len(cs)-1; i++ {
			if i < 0 {
				continue
			}
			vols = append(vols, float64(cs[i].Volume))
		}
		mean, std := meanStd(vols)
		latestVol := cs[len(cs)-1].Volume
		z := 0.0
		if std > 0 {
			z = (float64(latestVol) - mean) / std
		}
		relPct := 0.0
		if mean > 0 {
			relPct = float64(latestVol) / mean * 100
		}
		whaleRes = signal.WhaleResult{Symbol: symbol, Volume: latestVol, MeanVol: mean, StdDev: std, ZScore: z, RelVolPct: relPct, IsWhale: z >= 3.0 || relPct >= 500}
		// regime from live candles
		var hist []map[string]any
		for _, c := range cs {
			hist = append(hist, map[string]any{"high": c.High, "low": c.Low, "close": c.Close})
		}
		atr, atrPct := calcATR(hist, 14)
		emaSlope := calcEMASlope(hist, 20)
		reg := signal.RegimeRanging
		if atrPct > 2.0 {
			reg = signal.RegimeVolatile
		} else if math.Abs(emaSlope) > 0.5 {
			reg = signal.RegimeTrending
		}
		regimeRes = signal.RegimeResult{Symbol: symbol, Regime: reg, ATR: atr, ATRPercent: atrPct, EMASlopePct: emaSlope}
		// orderbook live
		sClient := stockbit.New(cfg.StockbitBaseURL, cfg.StockbitToken)
		obSvc := orderbookapp.New(sClient)
		ob, err := obSvc.Get(ctx, symbol)
		if err == nil {
			imbalance = ob.Imbalance().Ratio
		}
		q, _ := qSvc.GetQuote(ctx, symbol)
		if q != nil {
			changePct = q.ChangePct
		}
	}
	decision := signal.DecisionWait
	conf := 0.55
	reason := "no strong signal"
	if whaleRes.IsWhale && regimeRes.Regime == signal.RegimeTrending && imbalance > 0.2 && changePct > 1 {
		decision = signal.DecisionBuy
		conf = 0.78
		reason = "whale + trending + bid dominance"
	} else if whaleRes.IsWhale && regimeRes.Regime == signal.RegimeVolatile {
		decision = signal.DecisionNoTrade
		conf = 0.3
		reason = "whale but volatile regime - fakeout risk"
	} else if imbalance > 0.3 && whaleRes.IsWhale {
		decision = signal.DecisionBuy
		conf = 0.65
		reason = "whale + bid dominance"
	} else if imbalance < -0.3 {
		decision = signal.DecisionNoTrade
		conf = 0.4
		reason = "ask dominance"
	}
	if mock {
		whaleRes.Source = "mock"
		regimeRes.Source = "mock"
	}
	res := signal.SignalResult{Symbol: symbol, Decision: decision, Confidence: conf, Whale: &whaleRes, Regime: &regimeRes, Imbalance: imbalance, ChangePct: changePct, Reason: reason, Source: "mock", Timestamp: time.Now()}
	if !mock {
		res.Source = "live"
	}
	outputJSON(res, compact)
	return 0
}

func meanStd(vals []float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var sq float64
	for _, v := range vals {
		d := v - mean
		sq += d * d
	}
	variance := sq / float64(len(vals))
	return mean, math.Sqrt(variance)
}

func calcATR(candles []map[string]any, period int) (float64, float64) {
	if len(candles) < period+1 {
		return 0, 0
	}
	var trs []float64
	for i := 1; i < len(candles); i++ {
		high := toFloatCandles(candles[i]["high"])
		low := toFloatCandles(candles[i]["low"])
		prevClose := toFloatCandles(candles[i-1]["close"])
		tr := math.Max(high-low, math.Max(math.Abs(high-prevClose), math.Abs(low-prevClose)))
		trs = append(trs, tr)
		if len(trs) > period {
			trs = trs[1:]
		}
	}
	if len(trs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range trs {
		sum += v
	}
	atr := sum / float64(len(trs))
	lastClose := toFloatCandles(candles[len(candles)-1]["close"])
	pct := 0.0
	if lastClose > 0 {
		pct = atr / lastClose * 100
	}
	return atr, pct
}

func calcEMASlope(candles []map[string]any, period int) float64 {
	if len(candles) < period+1 {
		return 0
	}
	var closes []float64
	for _, c := range candles {
		closes = append(closes, toFloatCandles(c["close"]))
	}
	ema := closes[0]
	k := 2.0 / float64(period+1)
	for i := 1; i < len(closes); i++ {
		ema = closes[i]*k + ema*(1-k)
	}
	prevEma := closes[len(closes)-period]
	if prevEma == 0 {
		return 0
	}
	return (ema - prevEma) / prevEma * 100
}

func toFloatCandles(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	default:
		return 0
	}
}

func runServe(cfg config.Config, rest []string) int {
	_, flags, _, _, _ := parseFlags(rest, map[string]string{"port": cfg.Port})
	port := cfg.Port
	if flags["port"] != "" {
		port = flags["port"]
	}
	// Redis session: saat start cek redis, jika valid pakai, jika hampir expired rotate via refresh
	rc := cache.New(cfg.RedisURL)
	if rc.Available() {
		if t, err := auth.LoadFromRedis(rc); err == nil && t.AccessToken != "" {
			if !auth.IsExpired(t, 5*time.Minute) {
				fmt.Fprintf(os.Stderr, "redis: session valid expires=%s remaining=%v\n", t.ExpiresAt.Format(time.RFC3339), time.Until(t.ExpiresAt).Round(time.Second))
			} else if t.RefreshToken != "" {
				fmt.Fprintln(os.Stderr, "redis: session hampir expired, rotate...")
				if nt, err := auth.Refresh(t); err == nil {
					_ = auth.SaveToRedis(rc, nt)
					_ = auth.Save(nt, "")
					fmt.Fprintf(os.Stderr, "redis: rotated new expiry %s\n", nt.ExpiresAt.Format(time.RFC3339))
				} else {
					fmt.Fprintf(os.Stderr, "redis: rotate gagal: %v\n", err)
				}
			} else {
				fmt.Fprintln(os.Stderr, "redis: session expired tanpa refresh_token, butuh indostock auth save")
			}
		} else {
			if t, err := auth.Load(); err == nil && t.AccessToken != "" {
				_ = auth.SaveToRedis(rc, t)
				fmt.Fprintf(os.Stderr, "redis: seeded dari file expires=%s\n", t.ExpiresAt.Format(time.RFC3339))
			}
		}
	} else {
		fmt.Fprintln(os.Stderr, "redis: tidak tersedia, fallback ke file token.json")
	}
	// Cron in-process: cek tiap jam, rotate jika <30m mau expired, dan tiap hari 04:00 WIB paksa cek
	wib, _ := time.LoadLocation("Asia/Jakarta")
	if wib == nil {
		wib = time.FixedZone("WIB", 7*3600)
	}
	c := cron.New(cron.WithLocation(wib))
	_, _ = c.AddFunc("0 * * * *", func() {
			rc2 := cache.New(cfg.RedisURL)
			if !rc2.Available() {
				return
			}
			t, err := auth.LoadFromRedis(rc2)
			if err != nil || t.RefreshToken == "" {
				return
			}
			if auth.IsExpired(t, 30*time.Minute) {
				log.Printf("cron: token will expire in %v, refreshing...", time.Until(t.ExpiresAt))
				if nt, err := auth.Refresh(t); err == nil {
					_ = auth.SaveToRedis(rc2, nt)
					_ = auth.Save(nt, "")
					log.Printf("cron: refreshed new expiry %s", nt.ExpiresAt.Format(time.RFC3339))
				} else {
					log.Printf("cron: refresh failed: %v", err)
				}
			}
		})
	c.Start()
	defer c.Stop()
	fmt.Fprintf(os.Stderr, "indostock serve listening on :%s (REDIS_URL=%s DATABASE_URL=%s) cron:WIB hourly+04:00\n", port, cfg.RedisURL, cfg.DatabaseURL)
	mux := httpNewMux(cfg)
	addr := ":" + port
	if err := httpListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
		return 1
	}
	return 0
}

func httpNewMux(cfg config.Config) *httpMux {
	m := &httpMux{cfg: cfg}
	return m
}

type httpMux struct{ cfg config.Config }

func (m *httpMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	path := r.URL.Path
	q := r.URL.Query()
	symbol := strings.ToUpper(q.Get("symbol"))
	if symbol == "" {
		symbol = strings.ToUpper(q.Get("code"))
	}
	switch {
	case path == "/health" || path == "/":
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "indostock", "redis": m.cfg.RedisURL, "db": m.cfg.DatabaseURL, "time": time.Now()})
	case strings.HasPrefix(path, "/api/quote"):
		if symbol == "" {
			symbol = "BBCA"
		}
		if q.Get("mock") == "1" || q.Get("mock") == "true" {
			json.NewEncoder(w).Encode(mockQuote(symbol))
			return
		}
		client := yahoo.New(m.cfg.YahooBaseURL)
		svc := quote.New(client)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		qt, err := svc.GetQuote(ctx, symbol)
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(qt)
	case strings.HasPrefix(path, "/api/signal"):
		if symbol == "" {
			symbol = "BBCA"
		}
		// for HTTP, always mock-friendly
		json.NewEncoder(w).Encode(map[string]any{"symbol": symbol, "note": "use CLI signal for full whale+regime, this is health endpoint", "whale_mock": mockQuote(symbol)})
	default:
		json.NewEncoder(w).Encode(map[string]any{"endpoints": []string{"/health", "/api/quote?symbol=BBCA&mock=1", "/api/signal?symbol=BBCA"}, "cli": "indostock whale BBCA --mock"})
	}
}

func httpListenAndServe(addr string, handler http.Handler) error {
	srv := &httpServer{Addr: addr, Handler: handler}
	return srv.ListenAndServe()
}

type httpServer struct {
	Addr    string
	Handler http.Handler
}

func (s *httpServer) ListenAndServe() error {
	ln, err := netListen("tcp", s.Addr)
	if err != nil {
		return err
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleConn(conn, s.Handler)
	}
}

func netListen(network, addr string) (netListener, error) {
	return net.Listen(network, addr)
}

type netListener interface {
	Accept() (net.Conn, error)
	Close() error
	Addr() net.Addr
}

func handleConn(conn net.Conn, handler http.Handler) {
	defer conn.Close()
	br := bufioNewReader(conn)
	req, err := httpReadRequest(br)
	if err != nil {
		return
	}
	rw := &respWriter{conn: conn, header: make(http.Header)}
	handler.ServeHTTP(rw, req)
	rw.flush()
}

type respWriter struct {
	conn   net.Conn
	header http.Header
	status int
	body   []byte
}

func (w *respWriter) Header() http.Header { return w.header }
func (w *respWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}
func (w *respWriter) WriteHeader(status int) { w.status = status }
func (w *respWriter) flush() {
	if w.status == 0 {
		w.status = 200
	}
	statusText := httpStatusText(w.status)
	fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", w.status, statusText)
	for k, vs := range w.header {
		for _, v := range vs {
			fmt.Fprintf(w.conn, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(w.conn, "Content-Length: %d\r\nConnection: close\r\n\r\n", len(w.body))
	w.conn.Write(w.body)
}

func httpStatusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 500:
		return "Internal Server Error"
	default:
		return "OK"
	}
}

func bufioNewReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }
func httpReadRequest(r *bufio.Reader) (*http.Request, error) { return http.ReadRequest(r) }
