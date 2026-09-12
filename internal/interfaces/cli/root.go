package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/robfig/cron/v3"
	finapp "indonesia-stock/internal/application/financial"
	orderbookapp "indonesia-stock/internal/application/orderbook"
	"indonesia-stock/internal/application/quote"
	findomain "indonesia-stock/internal/domain/financial"
	"indonesia-stock/internal/domain/orderbook"
	"indonesia-stock/internal/domain/signal"
	"indonesia-stock/internal/domain/stock"
	"indonesia-stock/internal/infrastructure/cache"
	"indonesia-stock/internal/infrastructure/scraper/idx"
	"indonesia-stock/internal/infrastructure/scraper/rti"
	sa "indonesia-stock/internal/infrastructure/scraper/stockanalysis"
	"indonesia-stock/internal/infrastructure/scraper/stockbit"
	"indonesia-stock/internal/infrastructure/scraper/yahoo"
	"indonesia-stock/pkg/auth"
	"indonesia-stock/pkg/config"
	"log"
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
	case "fundamentals", "fundas", "val":
		return runFundamentals(cfg, rest)
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

// stringFlags accept both "--name value" and "--name=value" forms.
var stringFlags = []string{"interval", "range", "depth", "period", "type", "year", "quarter", "z", "window", "port"}

// boolFlags are plain on/off switches.
var boolFlags = []string{"watch", "mock", "debug", "compact", "csv", "json"}

// parseFlagArgs scans rest into (name → value) with defaults pre-applied.
// Positional non-flag words are returned separately (first = SYMBOL).
// isStringFlag reports whether name is a string-valued flag, i.e. it consumes
// the next non-flag token as its value.
func isStringFlag(name, next string, nextExists bool) bool {
	if !nextExists || strings.HasPrefix(next, "-") {
		return false
	}
	if name == "json" {
		return true // --json <bool> is accepted alongside --json=false
	}
	for _, known := range stringFlags {
		if name == known {
			return true
		}
	}
	return false
}

// applyBoolFlag marks a boolean flag as true for the long "--flag" form.
// The explicit "--json=false" form is handled at parseFlags level, not here.
func applyBoolFlag(a, name string, vals map[string]string) {
	if strings.HasPrefix(a, "--json=") || !strings.HasPrefix(a, "--") {
		return
	}
	if _, isBool := vals[name]; isBool {
		vals[name] = "true"
	}
}

// initFlagVals seeds vals with defaults then boolean false defaults.
func initFlagVals(defaults map[string]string) map[string]string {
	vals := make(map[string]string, len(defaults)+len(boolFlags))
	for k, v := range defaults {
		vals[k] = v
	}
	for _, b := range boolFlags {
		if _, ok := vals[b]; !ok {
			vals[b] = "false"
		}
	}
	return vals
}

// scanTokens walks args, filling vals/symbol/positional. string-value flags
// consume the following token; booleans flip on the long "--flag" form.
func scanTokens(rest []string, vals map[string]string) (symbol string, positional []string) {
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "-") {
			if symbol == "" {
				symbol = strings.ToUpper(a)
			} else {
				positional = append(positional, a)
			}
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.Index(name, "="); eq != -1 {
			vals[name[:eq]] = name[eq+1:]
			continue
		}
		nextExists := i+1 < len(rest)
		var next string
		if nextExists {
			next = rest[i+1]
		}
		if isStringFlag(name, next, nextExists) {
			i++
			vals[name] = next
			continue // string flag took its value; never treat as boolean
		}
		applyBoolFlag(a, name, vals)
	}
	return symbol, positional
}

func parseFlagArgs(rest []string, defaults map[string]string) (string, map[string]string, []string) {
	vals := initFlagVals(defaults)
	symbol, positional := scanTokens(rest, vals)
	return symbol, vals, positional
}

func parseFlags(rest []string, defaults map[string]string) (symbol string, flags map[string]string, compact, jsonOut, csvOut bool) {
	// manual flag parser: supports flags before or after SYMBOL (AI agents call both forms)
	// Special-case boolean semantics that the generic scanner can't express
	// (--json=false), then delegate.
	for _, a := range rest {
		if a == "--json=false" || a == "--json=0" {
			defaults["json"] = "false"
		}
	}
	symbol, vals, _ := parseFlagArgs(rest, defaults)
	jsonOut = vals["json"] != "false"
	compact = vals["compact"] == "true"
	csvOut = vals["csv"] == "true"
	return symbol, vals, compact, jsonOut, csvOut
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

// srcName reports which store the startup session came from.
func srcName(fileTok, redisTok *auth.StoredToken) string {
	if fileTok != nil {
		return "token.json"
	}
	if redisTok != nil {
		return "redis"
	}
	return "none"
}

// runFundamentals fetches fundamental metrics for IDX symbols from
// stockanalysis.com (S&P Global data), cached 24h in Redis.
//
//	indostock fundamentals BBCA [--compact|--json]
//	indostock fundamentals BBCA BBRI TLKM
//	INDOSTOCK_NO_CACHE=1 indostock fundamentals BBCA   (bypass cache)
//
// parseFundArgs splits args into symbols and compact flag.
func parseFundArgs(rest []string) (syms []string, compact bool) {
	syms = make([]string, 0, 4)
	for _, a := range rest {
		switch {
		case a == "--compact" || a == "--json" || a == "-c":
			compact = true
		case strings.HasPrefix(a, "--"):
			// ignore unknown flags silently (parseFlags-compatible)
		default:
			if !strings.HasPrefix(a, "-") {
				syms = append(syms, strings.ToUpper(strings.TrimSpace(a)))
			}
		}
	}
	return syms, compact
}

// cachedFundamentals returns the cached fundamentals for sym, or nil.
func cachedFundamentals(rc *cache.Client, key string) *sa.Fundamentals {
	if rc == nil {
		return nil
	}
	raw, err := rc.Get(key)
	if err != nil || raw == "" {
		return nil
	}
	f := &sa.Fundamentals{}
	if json.Unmarshal([]byte(raw), f) != nil || f.PE == "" {
		return nil
	}
	return f
}

// printOrEmit outputs f as compact JSON or a human table.
func printOrEmit(f *sa.Fundamentals, compact bool, raw string) {
	if compact {
		if raw == "" {
			b, _ := json.Marshal(f)
			raw = string(b)
		}
		fmt.Println(raw)
		return
	}
	printFundamentals(f)
}

// fetchAndCacheFundamentals fetches fresh data for sym and populates the cache.
func fetchAndCacheFundamentals(client *sa.Client, rc *cache.Client, key, sym string, rcOK bool) (*sa.Fundamentals, error) {
	f, err := client.FetchStatistics(sym)
	if err != nil {
		return nil, err
	}
	if rcOK {
		if b, jerr := json.Marshal(f); jerr == nil {
			_ = rc.SetEx(key, string(b), 24*time.Hour)
		}
	}
	return f, nil
}

func runFundamentals(cfg config.Config, rest []string) int {
	syms, compact := parseFundArgs(rest)
	if len(syms) == 0 {
		fmt.Fprintln(os.Stderr, "fundamentals: need SYMBOL, e.g. indostock fundamentals BBCA")
		return 1
	}
	noCache := os.Getenv("INDOSTOCK_NO_CACHE") == "1"
	client := sa.New()
	var rc *cache.Client
	if !noCache {
		rc = cache.New(cfg.RedisURL)
	}
	rcOK := rc != nil && rc.Available()

	exit := 0
	for _, sym := range syms {
		key := "indostock:fundamentals:" + sym
		if rcOK {
			if f := cachedFundamentals(rc, key); f != nil {
				printOrEmit(f, compact, "")
				continue
			}
		}
		f, err := fetchAndCacheFundamentals(client, rc, key, sym, rcOK)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fundamentals %s: %v\n", sym, err)
			exit = 1
			continue
		}
		printOrEmit(f, compact, "")
	}
	return exit
}

func printFundamentals(f *sa.Fundamentals) {
	fmt.Printf("%s  (per %s, sumber %s)\n", f.Symbol, f.AsOf, f.Source)
	rows := []struct{ k, v string }{
		{"PE / Fwd PE", strings.TrimSpace(f.PE + " / " + f.PEForward)},
		{"PBV / PEG", strings.TrimSpace(f.PBV + " / " + f.PEG)},
		{"ROE / ROA", strings.TrimSpace(f.ROE + " / " + f.ROA)},
		{"Margin (op/profit)", strings.TrimSpace(f.OpMargin + " / " + f.ProfMargin)},
		{"EPS TTM", f.EPS},
		{"Revenue TTM", f.Revenue},
		{"Net Income TTM", f.NetIncome},
		{"Market Cap", f.MarketCap},
		{"Net Cash / D-E", strings.TrimSpace(f.NetCash + " / " + f.DebtEquity)},
		{"BVPS", f.BookPerSh},
		{"Div/sh / Yield / Payout", strings.TrimSpace(f.DivPerSh + " / " + f.DivYield + " / " + f.Payout)},
		{"Beta", f.Beta},
		{"Analyst", strings.TrimSpace(f.Consensus + " TP " + f.PriceTP + " (upside " + f.UpsidePct + ")")},
	}
	for _, r := range rows {
		v := strings.TrimSpace(r.v)
		if v != "" && v != "/" {
			fmt.Printf("  %-24s %s\n", r.k, v)
		}
	}
}

// clipDepth trims bids/asks to the requested depth (0 = no clipping).
func clipDepth[T ~[]E, E any](levels T, depth int) T {
	if depth <= 0 || len(levels) <= depth {
		return levels
	}
	return levels[:depth]
}

// runOrderbookMock serves the mock orderbook once, or loops in watch mode.
func runOrderbookMock(symbol string, depth, interval int, watch, compact bool) {
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
}

// orderbookErrorHint builds the actionable error payload for auth failures.
func orderbookErrorHint(err error, symbol string, tokenSet bool, compact bool) {
	hint := "save tokens from browser login via 'indostock auth save --token <JWT> --refresh <JWT> (or --stdin), then indostock auth status --json to verify.' (ADR-0009)"
	if !tokenSet {
		hint += " | ADR-0009: server logins trigger OTP and fail; use browser token capture"
	}
	outputJSON(map[string]any{
		"error": err.Error(), "symbol": symbol, "hint": hint,
		"endpoint": "https://exodus.stockbit.com/company-price-feed/v2/orderbook/companies/" + symbol,
	}, compact)
}

func runOrderbook(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"interval": "2", "depth": "10"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "orderbook: need SYMBOL, e.g. indostock orderbook BBCA --depth 10")
		return 1
	}
	depth := 10
	fmt.Sscan(flags["depth"], &depth)
	mock := flags["mock"] == "true"
	interval := 2
	fmt.Sscan(flags["interval"], &interval)

	if mock {
		runOrderbookMock(symbol, depth, interval, flags["watch"] == "true", compact)
		return 0
	}

	client := stockbit.New(cfg.StockbitBaseURL, cfg.StockbitToken)
	client.OnAuthRejected = func() string { tok, _ := auth.ForceRefresh(); return tok }
	svc := orderbookapp.New(client)

	if flags["watch"] == "true" {
		return streamOrderbook(svc, symbol, interval, depth)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ob, err := svc.Get(ctx, symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orderbook error: %v\n", err)
		orderbookErrorHint(err, symbol, cfg.StockbitToken != "", compact)
		return 1
	}
	ob.Bids = clipDepth(ob.Bids, depth)
	ob.Asks = clipDepth(ob.Asks, depth)
	outputJSON(enrichOrderbook(ob), compact)
	return 0
}

// streamOrderbook consumes the live orderbook stream until the channel closes.
func streamOrderbook(svc *orderbookapp.Service, symbol string, interval, depth int) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := svc.Stream(ctx, symbol, interval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
		return 1
	}
	for ob := range ch {
		ob.Bids = clipDepth(ob.Bids, depth)
		ob.Asks = clipDepth(ob.Asks, depth)
		outputJSON(enrichOrderbook(&ob), true)
	}
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
	sClient.OnAuthRejected = func() string { tok, _ := auth.ForceRefresh(); return tok }
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

// sideStats walks one side (bids/asks) of the raw JSON orderbook and
// returns (best price, total lot volume).
func sideStats(levels []any) (best float64, total int64) {
	for _, b := range levels {
		mm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if p, ok := mm["price"].(float64); ok && best == 0 {
			best = p
		}
		if l, ok := mm["lot"].(float64); ok {
			total += int64(l)
		}
	}
	return best, total
}

func enrichOrderbook(v any) any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	bidsRaw, _ := m["bids"].([]any)
	asksRaw, _ := m["asks"].([]any)
	bestBid, bidVol := sideStats(bidsRaw)
	bestAsk, askVol := sideStats(asksRaw)
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
		for _, c := range symbol {
			h = h*31 + int(c)
		}
		if h < 0 {
			h = -h
		}
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
	case "1d":
		count = 7
	case "5d":
		count = 5
	case "1mo":
		count = 22
	case "3mo":
		count = 60
	case "1y":
		count = 250
	case "5y":
		count = 60
	}
	if count > 120 {
		count = 120
	}
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

// whaleInputs bundles candle history + latest volume from either mock or live.
type whaleInputs struct {
	candles   []map[string]any
	latestVol int64
	source    string
}

func gatherWhaleInputs(cfg config.Config, symbol string, mock, compact bool) (whaleInputs, int) {
	var wi whaleInputs
	if mock {
		wi.candles = mockHistory(symbol, "1d", "3mo")
		if n := len(wi.candles); n > 0 {
			if v, ok := wi.candles[n-1]["volume"].(int64); ok {
				wi.latestVol = v
			}
		}
		// Sonar: no hidden random multiplier — mock volumes are already
		// randomized by mockHistory, whale detection must stay deterministic
		// on a given dataset.
		wi.source = "mock"
		return wi, 0
	}
	client := yahoo.New(cfg.YahooBaseURL)
	svc := quote.New(client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, err := svc.GetHistory(ctx, symbol, "1d", "3mo")
	if err != nil {
		fmt.Fprintf(os.Stderr, "whale history error: %v\n", err)
		outputJSON(map[string]any{"error": err.Error(), "hint": "gunakan --mock"}, compact)
		return wi, 1
	}
	for _, c := range cs {
		wi.candles = append(wi.candles, map[string]any{"volume": c.Volume, "close": c.Close})
	}
	if len(cs) > 0 {
		wi.latestVol = cs[len(cs)-1].Volume
	}
	wi.source = "live"
	return wi, 0
}

// whaleWindowVolumes extracts the trailing window volumes before the latest.
func whaleWindowVolumes(candles []map[string]any, window int) []float64 {
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
	vols := make([]float64, 0, window)
	for i := start; i < len(candles)-1; i++ {
		switch v := candles[i]["volume"].(type) {
		case int64:
			vols = append(vols, float64(v))
		case float64:
			vols = append(vols, v)
		}
	}
	return vols
}

// whaleVerdict derives z-score, relative volume and the whale decision.
func whaleVerdict(latestVol int64, vols []float64, zThresh float64) (z, relPct, mean, std, confidence float64, isWhale bool, reason string) {
	mean, std = meanStd(vols)
	if std > 0 {
		z = (float64(latestVol) - mean) / std
	}
	if mean > 0 {
		relPct = float64(latestVol) / mean * 100
	}
	isWhale = z >= zThresh || relPct >= 500
	reason = "no whale"
	if !isWhale {
		return z, relPct, mean, std, 0, false, reason
	}
	confidence = 70 + (z-zThresh)*15
	if relPct > 500 {
		confidence += 5
	}
	if confidence > 99 {
		confidence = 99
	}
	reason = fmt.Sprintf("whale detected Z=%.2f RelVol=%.0f%%", z, relPct)
	return z, relPct, mean, std, confidence, true, reason
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

	wi, code := gatherWhaleInputs(cfg, symbol, flags["mock"] == "true", compact)
	if code != 0 {
		return code
	}
	vols := whaleWindowVolumes(wi.candles, window)
	z, relPct, mean, std, confidence, isWhale, reason := whaleVerdict(wi.latestVol, vols, zThresh)
	res := signal.WhaleResult{
		Symbol: symbol, Volume: wi.latestVol, MeanVol: mean, StdDev: std, ZScore: z, RelVolPct: relPct, IsWhale: isWhale, Confidence: confidence, Reason: reason, Source: wi.source, Timestamp: time.Now(),
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

// candleMap converts domain candles to the map shape used by calcATR/EMA.
func candleMap(cs []stock.Candle) []map[string]any {
	hist := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		hist = append(hist, map[string]any{"high": c.High, "low": c.Low, "close": c.Close})
	}
	return hist
}

// volumeStats computes (mean, std, z, relPct) of the latest volume vs the
// trailing window (Sonar: complexity — shared by whale & signal paths).
func volumeStats(vols []float64, latestVol int64) (mean, std, z, relPct float64) {
	mean, std = meanStd(vols)
	if std > 0 {
		z = (float64(latestVol) - mean) / std
	}
	if mean > 0 {
		relPct = float64(latestVol) / mean * 100
	}
	return mean, std, z, relPct
}

// trailingVolumes collects up to 60 volumes before the latest entry.
func trailingVolumes(candles []map[string]any) []float64 {
	var vols []float64
	for i := len(candles) - 61; i < len(candles)-1; i++ {
		if i < 0 {
			continue
		}
		switch v := candles[i]["volume"].(type) {
		case int64:
			vols = append(vols, float64(v))
		case float64:
			vols = append(vols, v)
		}
	}
	return vols
}

// regimeFromMetrics maps ATR%/EMA-slope onto a regime label.
func regimeFromMetrics(atrPct, emaSlope float64) signal.Regime {
	if atrPct > 2.0 {
		return signal.RegimeVolatile
	}
	if math.Abs(emaSlope) > 0.5 {
		return signal.RegimeTrending
	}
	return signal.RegimeRanging
}

// decideSignal combines whale + regime + orderflow into a decision.
func decideSignal(whaleRes signal.WhaleResult, regimeRes signal.RegimeResult, imbalance, changePct float64) (signal.SignalDecision, float64, string) {
	decision := signal.DecisionWait
	conf := 0.55
	reason := "no strong signal"
	switch {
	case whaleRes.IsWhale && regimeRes.Regime == signal.RegimeTrending && imbalance > 0.2 && changePct > 1:
		decision, conf, reason = signal.DecisionBuy, 0.78, "whale + trending + bid dominance"
	case whaleRes.IsWhale && regimeRes.Regime == signal.RegimeVolatile:
		decision, conf, reason = signal.DecisionNoTrade, 0.3, "whale but volatile regime - fakeout risk"
	case imbalance > 0.3 && whaleRes.IsWhale:
		decision, conf, reason = signal.DecisionBuy, 0.65, "whale + bid dominance"
	case imbalance < -0.3:
		decision, conf, reason = signal.DecisionNoTrade, 0.4, "ask dominance"
	}
	return decision, conf, reason
}

// gatherMockInputs fills whale/regime/imbalance/change from mock generators.
func gatherMockInputs(symbol string) (signal.WhaleResult, signal.RegimeResult, float64, float64) {
	hist := mockHistory(symbol, "1d", "3mo")
	vols := trailingVolumes(hist)
	latestVol, _ := hist[len(hist)-1]["volume"].(int64)
	mean, std, z, relPct := volumeStats(vols, latestVol)
	whaleRes := signal.WhaleResult{Symbol: symbol, Volume: latestVol, MeanVol: mean, StdDev: std, ZScore: z, RelVolPct: relPct, IsWhale: z >= 3.0 || relPct >= 500}
	atr, atrPct := calcATR(hist, 14)
	emaSlope := calcEMASlope(hist, 20)
	regimeRes := signal.RegimeResult{Symbol: symbol, Regime: regimeFromMetrics(atrPct, emaSlope), ATR: atr, ATRPercent: atrPct, EMASlopePct: emaSlope}
	ob := mockOrderbook(symbol, 10)
	q := mockQuote(symbol)
	return whaleRes, regimeRes, ob.Imbalance().Ratio, q["change_pct"].(float64)
}

// gatherLiveInputs fills whale/regime/imbalance/change from live services.
func gatherLiveInputs(cfg config.Config, symbol string) (signal.WhaleResult, signal.RegimeResult, float64, float64, error) {
	yClient := yahoo.New(cfg.YahooBaseURL)
	qSvc := quote.New(yClient)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, err := qSvc.GetHistory(ctx, symbol, "1d", "3mo")
	if err != nil {
		return signal.WhaleResult{}, signal.RegimeResult{}, 0, 0, err
	}
	latest := cs[len(cs)-1]
	vols := make([]float64, 0, 60)
	for i := len(cs) - 61; i < len(cs)-1; i++ {
		if i >= 0 {
			vols = append(vols, float64(cs[i].Volume))
		}
	}
	mean, std, z, relPct := volumeStats(vols, latest.Volume)
	whaleRes := signal.WhaleResult{Symbol: symbol, Volume: latest.Volume, MeanVol: mean, StdDev: std, ZScore: z, RelVolPct: relPct, IsWhale: z >= 3.0 || relPct >= 500}
	hist := candleMap(cs)
	atr, atrPct := calcATR(hist, 14)
	emaSlope := calcEMASlope(hist, 20)
	regimeRes := signal.RegimeResult{Symbol: symbol, Regime: regimeFromMetrics(atrPct, emaSlope), ATR: atr, ATRPercent: atrPct, EMASlopePct: emaSlope}
	sClient := stockbit.New(cfg.StockbitBaseURL, cfg.StockbitToken)
	sClient.OnAuthRejected = func() string { tok, _ := auth.ForceRefresh(); return tok }
	obSvc := orderbookapp.New(sClient)
	var imbalance, changePct float64
	if ob, err := obSvc.Get(ctx, symbol); err == nil {
		imbalance = ob.Imbalance().Ratio
	}
	if q, _ := qSvc.GetQuote(ctx, symbol); q != nil {
		changePct = q.ChangePct
	}
	return whaleRes, regimeRes, imbalance, changePct, nil
}

func runSignal(cfg config.Config, rest []string) int {
	symbol, flags, compact, _, _ := parseFlags(rest, map[string]string{"z": "3.0"})
	if symbol == "" {
		fmt.Fprintln(os.Stderr, "signal: need SYMBOL, e.g. indostock signal BBCA --mock")
		return 1
	}
	mock := flags["mock"] == "true"
	var whaleRes signal.WhaleResult
	var regimeRes signal.RegimeResult
	var imbalance, changePct float64
	if mock {
		whaleRes, regimeRes, imbalance, changePct = gatherMockInputs(symbol)
	} else {
		var err error
		whaleRes, regimeRes, imbalance, changePct, err = gatherLiveInputs(cfg, symbol)
		if err != nil {
			outputJSON(map[string]any{"error": err.Error()}, compact)
			return 1
		}
	}
	decision, conf, reason := decideSignal(whaleRes, regimeRes, imbalance, changePct)
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

// restoreSession validates the stored token at startup: valid → log, near
// expiry → rotate, unusable → seed Redis from token.json (Sonar: cognitive
// complexity — extracted from runServe).
// restoreSession is intentionally simple: pick the best token source and act.
// Kept flat by delegating each case to a helper below.
func restoreSession(rc *cache.Client) {
	if !rc.Available() {
		fmt.Fprintln(os.Stderr, "redis: tidak tersedia, fallback ke file token.json")
		return
	}
	// prefer token.json (single source of truth); Redis cache only helps
	// if the file is unreadable. Rotation itself is lock-serialized
	// inside auth.Refresh().
	tfTok := loadFileToken()
	rt, _ := auth.LoadFromRedis(rc)
	src := tfTok
	if src == nil {
		src = rt
	}
	switch {
	case isSessionValid(src):
		fmt.Fprintf(os.Stderr, "session valid expires=%s remaining=%v (src=%s)\n", src.ExpiresAt.Format(time.RFC3339), time.Until(src.ExpiresAt).Round(time.Second), srcName(tfTok, rt))
	case hasRefreshablePair(src):
		rotateAndServe(rc, src)
	case src != nil && src.AccessToken != "":
		fmt.Fprintln(os.Stderr, "session expired tanpa refresh_token, butuh indostock auth save")
	default:
		seedRedisFromFile(rc, tfTok, rt)
	}
}

func loadFileToken() *auth.StoredToken {
	tf := auth.FindTokenFile()
	if tf == "" {
		return nil
	}
	tt, err := auth.LoadFrom(tf)
	if err != nil || tt.AccessToken == "" {
		return nil
	}
	return tt
}

func isSessionValid(t *auth.StoredToken) bool {
	return t != nil && t.AccessToken != "" && !auth.IsExpired(t, 5*time.Minute)
}

func hasRefreshablePair(t *auth.StoredToken) bool {
	return t != nil && t.AccessToken != "" && t.RefreshToken != ""
}

func rotateAndServe(rc *cache.Client, src *auth.StoredToken) {
	fmt.Fprintln(os.Stderr, "session hampir expired, rotate...")
	nt, err := auth.Refresh(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rotate gagal: %v\n", err)
		return
	}
	_ = auth.SaveToRedis(rc, nt)
	_ = auth.Save(nt, "")
	fmt.Fprintf(os.Stderr, "rotated new expiry %s\n", nt.ExpiresAt.Format(time.RFC3339))
}

// seedRedisFromFile populates Redis from token.json when neither source was
// usable directly (file may hold an AT-only pair).
func seedRedisFromFile(rc *cache.Client, tfTok, rt *auth.StoredToken) {
	if tfTok != nil || rt != nil {
		return
	}
	t, err := auth.Load()
	if err != nil || t.AccessToken == "" {
		return
	}
	_ = auth.SaveToRedis(rc, t)
	fmt.Fprintf(os.Stderr, "redis: seeded dari file expires=%s\n", t.ExpiresAt.Format(time.RFC3339))
}

// loadCronToken picks the freshest stored token for the hourly cron: Redis
// first, falling back to token.json (source of truth — Sonar complexity
// extraction).
func loadCronToken(rc *cache.Client) *auth.StoredToken {
	if rc.Available() {
		if tt, err := auth.LoadFromRedis(rc); err == nil && tt != nil && tt.RefreshToken != "" {
			return tt
		}
	}
	if p := auth.FindTokenFile(); p != "" {
		if tt, err := auth.LoadFrom(p); err == nil && tt != nil && tt.RefreshToken != "" {
			log.Printf("cron: redis key missing/expired, using token.json (rt until %s)", rtBatteryStr(tt))
			return tt
		}
	}
	return nil
}

func rtBatteryStr(t *auth.StoredToken) string {
	if e, _ := auth.RTBattery(t); !e.IsZero() {
		return e.Format(time.RFC3339)
	}
	return "unknown"
}

// rotateIfExpiring refreshes the token when it will expire within 30m.
func rotateIfExpiring(rc *cache.Client, t *auth.StoredToken) {
	if !auth.IsExpired(t, 30*time.Minute) {
		return
	}
	log.Printf("cron: token will expire in %v, refreshing...", time.Until(t.ExpiresAt))
	if nt, err := auth.Refresh(t); err == nil {
		_ = auth.SaveToRedis(rc, nt)
		_ = auth.Save(nt, "")
		log.Printf("cron: refreshed new expiry %s", nt.ExpiresAt.Format(time.RFC3339))
	} else {
		log.Printf("cron: refresh failed: %v", err)
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
	restoreSession(rc)
	// Cron in-process: cek tiap jam, rotate jika <30m mau expired, dan tiap hari 04:00 WIB paksa cek
	wib, _ := time.LoadLocation("Asia/Jakarta")
	if wib == nil {
		wib = time.FixedZone("WIB", 7*3600)
	}
	c := cron.New(cron.WithLocation(wib))
	_, _ = c.AddFunc("0 * * * *", func() {
		rc2 := cache.New(cfg.RedisURL)
		// source of truth is token.json; Redis is only a read cache.
		// If the Redis key expired (e.g. service down > 24h) we MUST fall
		// back to the file — otherwise the healthy refresh token in the
		// file quietly rots to death while cron "returns" every hour.
		if t := loadCronToken(rc2); t != nil {
			rotateIfExpiring(rc2, t)
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

func bufioNewReader(c net.Conn) *bufio.Reader                { return bufio.NewReader(c) }
func httpReadRequest(r *bufio.Reader) (*http.Request, error) { return http.ReadRequest(r) }
