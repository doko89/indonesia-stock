# indonesia-stock — Go DDD + Orderbook CLI for AI Trading Agents

Rewrite dari `example/indonesia-stocks-scraper` (Python + Selenium) ke **Go** dengan struktur **Domain-Driven Design** + fitur **Orderbook** (tidak ada di example). CLI di-desain untuk **AI trading agent**: output JSON ke `stdout`, log ke `stderr`, exit code deterministik, `--compact` NDJSON.

> **Status deploy:** `ubuntu@ai.nex.my.id:/apps/indostock` (arm64), `systemd indostock.service :18080`, `/usr/local/bin/indostock`, Redis + TimescaleDB sebagai service. Auto-rotate Stockbit token via Redis + Go cron.

---

## 1. Semua Fitur Example Sudah Ada?

**Ya — 100% + tambahan.** Perbandingan:

| Fitur Example (Python) | Go DDD | Sumber Baru |
|---|---|---|
| Yahoo Finance quote & history | ✅ `quote`, `history`, `snapshot` | `yahoo` scraper (tanpa Selenium) |
| RTI financial report | ✅ `financial --period annual/quarterly --type income_statement/balance_sheet/cash_flow` | `rti` http langsung |
| IDX InlineXBRL | ✅ `idx --year --quarter` | `idx` downloader |
| — | 🆕 `orderbook --depth --watch` | `stockbit` exodus (Bearer + Redis auto-rotate) |
| — | 🆕 `whale --z --window` (Z-Score) | derived dari running trades |
| — | 🆕 `regime` (ATR/EMA) | derived |
| — | 🆕 `signal` (whale+regime+orderflow) | gabungan |
| — | 🆕 `snapshot` (quote+orderbook+imbalance+spread 1 JSON) | hemat 1 tool-call LLM |
| — | 🆕 `serve --port` HTTP (`/health`, `/api/quote`, `/api/signal`) | untuk service |
| — | 🆕 `auth status/refresh/save` | Redis + file token rotation |

Python example pakai Selenium (berat, butuh chromedriver). Go pakai `net/http` + regex — ringan, no browser.

---

## 2. Struktur DDD

```
cmd/indostock/main.go              # entry CLI (interface layer)
internal/
  domain/
    stock/        # Stock, Quote, Candle
    orderbook/    # Orderbook, Level, Imbalance (spread, mid, bid/ask)
    financial/    # Report, GeneralInfo
    quote/        # Repository contracts
    signal/       # Signal (whale+regime)
  application/
    stock/        # orchestrasi Stock
    quote/        # GetQuote, GetHistory (Yahoo)
    orderbook/    # Get, Stream (Stockbit)
    financial/    # GetReport, GetGeneralInfo (RTI)
  infrastructure/
    scraper/
      yahoo/      # Yahoo Finance chart API
      stockbit/   # Stockbit orderbook (exodus + auth)
      rti/        # RTI Business analytics
      idx/        # IDX InlineXBRL
    cache/        # redis.go (ZAdd/ZRange + Get/Set/SetEx/Del + mem fallback)
    store/        # timescale.go (lib/pq hypertable running_trades)
  interfaces/
    cli/          # root.go + auth.go (stdlib, no cobra)
pkg/
  config/         # env + tokenFromFile + Redis fallback
  auth/           # token.go (JWT expiry, StoredToken, RedisKey, Save/Load/Refresh)
  output/ logger/ # JSON helpers, stderr logger
bin/indostock, bin/indostock-linux-arm64
deploy/indostock.service, deploy/install-timescaledb.sh, deploy/install.sh
docker-compose.yml
```

Prinsip: `domain` tidak depend ke infra, `application` orchestrate, `infrastructure` implement repo, `interfaces/cli` hanya parse flag.

---

## 3. Orderbook — Fitur Baru

Example **tidak punya orderbook**. Implementasi:

- `internal/domain/orderbook/entity.go`: `Orderbook{Symbol, Bids[], Asks[]}` + `BestBid/Ask`, `Spread()`, `MidPrice()`, `Imbalance(){BidVolume,AskVolume,Ratio}`
- `internal/infrastructure/scraper/stockbit/client.go`: endpoint `https://exodus.stockbit.com/company-price-feed/v2/orderbook/companies/{SYMBOL}` + fallback `/v2.2/orderbook/{symbol}`, header `Authorization: Bearer <token>` + `Cookie`, `Origin/Referer`, `User-Agent Chrome/124`, parser `findDeep(bids/bid, asks/ask/offer)` untuk 3 bentuk JSON.
- Enriched output: `best_bid`, `best_ask`, `spread`, `mid_price`, `imbalance`.

Tanpa `STOCKBIT_TOKEN` endpoint return `401 "Silahkan update aplikasi"` — **bukan bug CLI, memang wajib Bearer**. Pakai `--mock` untuk test offline.

Contoh `orderbook --json`:

```json
{
  "symbol": "BBCA",
  "timestamp": "2026-09-02T11:00:00+07:00",
  "source": "stockbit",
  "bids": [{"price": 10125, "lot": 120}],
  "asks": [{"price": 10150, "lot": 200}],
  "best_bid": 10125, "best_ask": 10150,
  "spread": 25, "mid_price": 10137.5,
  "imbalance": {"bid_volume": 205, "ask_volume": 295, "ratio": -0.18}
}
```

---

## 4. Redis + TimescaleDB — Butuh Keduanya?

| Opsi | Kegunaan | Retensi | Kapan pakai |
|---|---|---|---|
| **Redis saja** | hot cache orderbook/quote, session token TTL | diset 1 jam (default) — bisa dipanjangkan, tapi RAM terbatas, hilang saat restart | prototyping / AI agent yang cuma butuh realtime 1 jam terakhir |
| **TimescaleDB saja** | hypertable `running_trades` (OHLCV per detik) | bulan/tahun (disk) | backtest, training model |
| **Keduanya (dipakai sekarang)** | Redis = L1 cepat (detik), Timescale = L2 tahan lama (hari/bulan) | Redis 1 jam → Timescale selamanya | **rekomendasi — sudah aktif di deploy** |

> Diskusi panjang: awalnya "tidak punya redis/db" → "jalan sebagai service dong?" → "retensi berapa?" → "Redis hanya 1 jam?" → akhirnya **pakai Redis + TimescaleDB sebagai service** (docker-compose + systemd). Redis untuk orderbook streaming, Timescale untuk `ZAdd` history.

`docker-compose.yml` sekarang:

```yaml
services:
  redis: image: redis:7, volume: redis_data
  timescaledb: image: timescale/timescaledb:latest-pg16, volume: timescale_data
  indostock: build ., depends_on [redis, timescaledb], env REDIS_URL/DATABASE_URL/STOCKBIT_TOKEN, ports 8080:8080
```

---

## 5. Auth & Auto-Rotate Token

Stockbit `access_token` (JWT) 24 jam, `refresh_token` 7 hari. Browser adalah **source of truth** (ADR-0009) — server tidak pernah `username/password` (akan kena OTP `9547/473439` + `INVALID_PARAMETER` karena IP server beda device).

Alur:

1. Login sekali di browser (Brave/Firefox) `krez.tk@gmail.com` → DevTools → Network `POST exodus.stockbit.com/login/v6/username` → copy `data.login.token_data.access.token` + `refresh.token`
2. `indostock auth save --token <access> --refresh <refresh>` atau `cat token.json | indostock auth save --stdin`
3. Disimpan ke `Redis indostock:auth:stockbit` (TTL = `exp`) + file `/apps/indostock/token.json` & `~/.config/indostock/token.json`
4. Saat `serve` start: cek `LoadFromRedis` jika valid `>5m` pakai, else `Refresh` via `POST /login/refresh` + `SaveToRedis/Save`
5. **Go cron** (bukan systemd timer) `robfig/cron` `Asia/Jakarta` `0 * * * *` tiap jam cek `IsExpired(30m)` → `Refresh` otomatis. User request: "jangan timer, simpan di redis, cron pakai go lib" — sudah.

```bash
indostock auth status --json   # lihat file + redis expiry, has_refresh
indostock auth refresh --json  # force rotate
indostock auth save --token eyJ... --refresh eyJ...
```

> Token terakhir (TCbFHUmtyloM2Y94, uid 8086488) `access exp 2026-09-03T06:36:17Z` `refresh exp 2026-09-09` — cron akan extend sebelum 30 menit expiry. Jika logout di browser, refresh akan 401 (sudah dites) — jangan logout.

Env fallback: `STOCKBIT_TOKEN` → `auth.Load()` file candidates → `tokenFromFile()`. `config.Load` prioritas env, jadi pastikan `/etc/profile.d/indostock.sh` + `token.conf` sinkron.

---

## 6. Instal & Jalan

```bash
# build lokal
GOCACHE=/tmp/gocache go build -buildvcs=false -o bin/indostock ./cmd/indostock
# build arm64 untuk server
GOOS=linux GOARCH=arm64 GOCACHE=/tmp/gocache go build -buildvcs=false -o bin/indostock-linux-arm64 ./cmd/indostock

# tanpa docker (butuh redis & pg lokal)
go run ./cmd/indostock quote BBCA --json
go run ./cmd/indostock orderbook BBCA --depth 5 --json --mock   # tanpa token
STOCKBIT_TOKEN=eyJ... go run ./cmd/indostock orderbook BBCA --depth 5 --json

# docker (redis+timescale sudah include)
docker compose up -d
curl http://localhost:8080/health

# systemd di ubuntu@ai.nex.my.id
./deploy/install.sh   # build arm64, scp ke /usr/local/bin/indostock, install redis+timescaledb, enable systemd
systemctl status indostock --no-pager
journalctl -u indostock -f
```

Env:

```bash
REDIS_URL=redis://localhost:6379/0
DATABASE_URL=postgres://indostock:indostock@localhost:5432/indostock?sslmode=disable
STOCKBIT_TOKEN=eyJ...   # atau via auth save
YAHOO_BASE_URL=https://query1.finance.yahoo.com
RTI_BASE_URL=https://analytics2.rti.co.id
PORT=18080   # 8080 bentrok search-api di server, pakai 18080
```

---

## 7. Perintah Lengkap

| Command | Sumber | Contoh |
|---|---|---|
| `quote <SYM>` | Yahoo | `indostock quote BBCA --json` |
| `history <SYM>` | Yahoo | `indostock history BBCA --interval 1d --range 3mo --json` |
| `orderbook <SYM>` | Stockbit | `indostock orderbook BBCA --depth 10 --json` |
| `financial <SYM>` | RTI | `indostock financial BBCA --period annual --type income_statement --json` |
| `idx <SYM>` | IDX | `indostock idx BBCA --year 2024 --quarter 1 --json --mock` |
| `whale <SYM>` | derived | `indostock whale BBCA --z 3 --window 60 --mock` |
| `regime <SYM>` | derived | `indostock regime BBCA --mock` |
| `signal <SYM>` | gabungan | `indostock signal BBCA --mock` |
| `snapshot <SYM>` | Yahoo+Stockbit | `indostock snapshot BBCA --json` |
| `auth status/refresh/save` | Stockbit | `indostock auth status --json` |
| `serve --port` | — | `indostock serve --port 18080` |

Flag global: `--json` (default true) → stdout JSON, `--compact` → 1 baris NDJSON, `--csv` (history), `--watch --interval N` (streaming), `--depth N`, `--period`, `--type`, `--year`, `--quarter`, `--z`, `--window`, `--mock` (offline), `--debug` (dump raw).

---

## 8. Untuk AI Agent Trading

### Tool definition (OpenAI/Claude)

```json
{
  "name": "indostock",
  "description": "Get Indonesia stock data. Use snapshot for quick decision, orderbook for depth, history for trend.",
  "parameters": {
    "type": "object",
    "properties": {
      "command": {"type": "string", "enum": ["quote","history","orderbook","snapshot","financial","whale","regime","signal"]},
      "symbol": {"type": "string", "description": "IDX code e.g. BBCA"},
      "interval": {"type": "string", "description": "1m,5m,1d,1wk"},
      "range": {"type": "string", "description": "1d,5d,1mo,3mo,1y"},
      "depth": {"type": "integer"}
    },
    "required": ["command","symbol"]
  }
}
```

### Pola

```python
import subprocess, json
def indostock(cmd, symbol, **opts):
    args = ["indostock", cmd, symbol, "--compact"]
    if opts.get("depth"): args += ["--depth", str(opts["depth"])]
    out = subprocess.check_output(args, stderr=subprocess.DEVNULL, text=True)
    return json.loads(out)

snap = indostock("snapshot", "BBCA")  # 1 call: quote+book+imbalance+signal
```

- Selalu `--compact` untuk hemat token.
- Jika `{"error": "401..."}` → fallback `quote`/`history` atau minta user set token. Mock tersedia untuk test.
- Streaming scalping: `indostock orderbook BBCA --watch --interval 1 --compact` → tail NDJSON hitung imbalance.

`AGENT.md` berisi panduan wrapper Python lengkap.

---

## 9. Deploy Detail (ubuntu@ai.nex.my.id)

- Path: `/apps/indostock` (Workdir), binary `/usr/local/bin/indostock` (arm64 9.0M) + symlink `/apps/indostock/bin/indostock`
- Service: `/etc/systemd/system/indostock.service` `ExecStart=/usr/local/bin/indostock serve --port 18080` `Environment=REDIS_URL/DATABASE_URL/PORT=18080`
- Token drop-in: `/etc/systemd/system/indostock.service.d/token.conf` (sinkron dengan `/etc/profile.d/indostock.sh`, `/apps/.env`, `/apps/token.json`)
- Deps: `redis-server` (PONG), `timescaledb 2-postgresql-16` (arm64, `hypertable running_trades`), `install-timescaledb.sh` tune & createdb
- Health: `curl :18080/health {"status":"ok",...}` — port 8080 bentrok `search-api`, jadi 18080
- Log: `journalctl -u indostock -f` lihat `redis: session valid ... cron:WIB`

---

## 10. Trade? Butuh Apa?

Token `access+refresh` sekarang **data-only**. Untuk trade (order placement) butuh `trading.stockbit.com` + PIN/2FA + sesi trading, tidak cukup JWT data. Opsi:

- Paper-trading DDD: `domain/order {Buy/Sell, paper=true}` → `trade buy BBCA --qty 100 --dry-run`
- Integrasi broker API (Stockbit tidak publish OAuth public).

Refresh token juga akan expired 7 hari — cron extend otomatis tiap 24 jam sebelum `access <30m`. Jika `refresh` 401 (logout/revoked) perlu login browser ulang.

---

## 11. Roadmap

- [ ] WebSocket RTI realtime (sekarang polling)
- [ ] Rate-limit & cache LRU untuk agent high-frequency
- [ ] MCP server mode (`indostock mcp`) untuk Claude/Cursor
- [ ] Paper trading + PnL journal di TimescaleDB

## Lisensi

Internal tool — pakai seperlunya untuk riset AI trading.
